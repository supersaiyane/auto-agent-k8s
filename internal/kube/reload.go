package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// Config reload, part 3 (PLAN-002 A2, A3.1): when a ConfigMap or Secret
// changes, restart the workloads that use the changed keys, one at a time,
// through the mutation gate. Only key names and hashes are kept; values
// never leave the informer cache.

// reloadVerifyTimeout is how long a reloaded StatefulSet or DaemonSet may
// take to become healthy; a Deployment uses its progress deadline.
const reloadVerifyTimeout = 10 * time.Minute

// reloadLogSize is how many reloads the Reloads tab keeps.
const reloadLogSize = 200

// workloadRef names one reloadable workload.
type workloadRef struct {
	Kind string // Deployment, StatefulSet, DaemonSet, CronJob
	Name string
}

func (w workloadRef) String() string { return strings.ToLower(w.Kind) + "/" + w.Name }

// ReloadOutcome is what happened to one workload in a reload.
type ReloadOutcome struct {
	Workload string `json:"workload"`
	Result   string `json:"result"` // up to date, simulated, suggested, blocked, restarted, healthy, failed, skipped
	Detail   string `json:"detail,omitempty"`
}

// ReloadRecord is one change and the workloads it reloaded (the Reloads tab).
type ReloadRecord struct {
	ID        int             `json:"id"`
	Timestamp time.Time       `json:"timestamp"`
	Namespace string          `json:"namespace"`
	Object    string          `json:"object"`      // configmap/app
	Keys      []string        `json:"changedKeys"` // names only
	Workloads []ReloadOutcome `json:"workloads"`
	Done      bool            `json:"done"`
}

// pendingChange is a change waiting out the debounce window.
type pendingChange struct {
	ns      string
	ref     objRef
	keys    map[string]bool
	objAnn  map[string]string
	data    map[string][]byte // the latest values, hashed only
	dueAt   time.Time
	startAt time.Time
}

// wave reloads the workloads one change affects, one at a time.
type wave struct {
	rec    *ReloadRecord
	change *pendingChange
	todo   []workloadRef
	// current is the workload restarted and not yet healthy.
	current   *workloadRef
	hash      string
	startedAt time.Time
}

// Reloader watches ConfigMaps (and Secrets, when enabled) and reloads the
// workloads that use them.
type Reloader struct {
	deps    *Deps
	cfg     config.Reload
	leading func() bool

	mu      sync.Mutex
	pending map[string]*pendingChange
	waves   []*wave
	blocked map[string]bool // namespace/workload/hash that stalled once
	log     []*ReloadRecord
	nextID  int
}

// NewReloader builds a reloader; leading reports whether this process is
// the leader, the only one that acts (constraint 3).
func NewReloader(deps *Deps, cfg config.Reload, leading func() bool) *Reloader {
	if cfg.On != reloadOnAlways {
		cfg.On = reloadOnAuto
	}
	return &Reloader{deps: deps, cfg: cfg, leading: leading,
		pending: map[string]*pendingChange{}, blocked: map[string]bool{}}
}

// Records returns copies of the reload log, newest first.
func (r *Reloader) Records() []ReloadRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReloadRecord, 0, len(r.log))
	for i := len(r.log) - 1; i >= 0; i-- {
		c := *r.log[i]
		c.Workloads = append([]ReloadOutcome(nil), c.Workloads...)
		out = append(out, c)
	}
	return out
}

// configMapChanged queues a ConfigMap update; every controller queues, so a
// leader change during the debounce loses nothing.
func (r *Reloader) configMapChanged(old, cur *corev1.ConfigMap) {
	r.queueChange(cur.Namespace, objRef{refConfigMap, cur.Name}, configMapData(old), configMapData(cur), cur.Annotations)
}

// secretChanged queues a Secret update.
func (r *Reloader) secretChanged(old, cur *corev1.Secret) {
	r.queueChange(cur.Namespace, objRef{refSecret, cur.Name}, old.Data, cur.Data, cur.Annotations)
}

func (r *Reloader) queueChange(ns string, ref objRef, before, after map[string][]byte, ann map[string]string) {
	if !r.deps.Policy().Watched(ns) {
		return
	}
	keys := changedKeys(before, after)
	if len(keys) == 0 {
		return // a metadata-only update, such as a label
	}
	now := r.deps.clock()
	r.mu.Lock()
	defer r.mu.Unlock()
	k := ns + "/" + ref.String()
	p := r.pending[k]
	if p == nil {
		p = &pendingChange{ns: ns, ref: ref, keys: map[string]bool{}, startAt: now}
		r.pending[k] = p
	}
	for _, key := range keys {
		p.keys[key] = true
	}
	p.objAnn, p.data, p.dueAt = ann, after, now.Add(r.cfg.Debounce)
	klog.V(3).Infof("reload: %s/%s changed keys %v; acting after %s", ns, ref, keys, r.cfg.Debounce)
}

// tick starts waves for changes past their debounce window and advances
// running waves. Every controller drops due changes; only the leader acts.
func (r *Reloader) reloadTick(ctx context.Context) {
	now := r.deps.clock()
	r.mu.Lock()
	var due []*pendingChange
	for k, p := range r.pending {
		if !now.Before(p.dueAt) {
			due = append(due, p)
			delete(r.pending, k)
		}
	}
	r.mu.Unlock()
	if !r.leading() {
		return
	}
	sort.Slice(due, func(a, b int) bool { return due[a].startAt.Before(due[b].startAt) })
	for _, p := range due {
		r.startWave(ctx, p)
	}
	r.mu.Lock()
	waves := append([]*wave(nil), r.waves...)
	r.mu.Unlock()
	for _, w := range waves {
		r.advanceWave(ctx, w)
	}
	r.mu.Lock()
	kept := r.waves[:0]
	for _, w := range r.waves {
		if !w.rec.Done {
			kept = append(kept, w)
		}
	}
	r.waves = kept
	r.mu.Unlock()
}

// startWave finds the workloads the change reloads and records the wave.
func (r *Reloader) startWave(ctx context.Context, p *pendingChange) {
	keys := make([]string, 0, len(p.keys))
	for k := range p.keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	targets := r.affectedWorkloads(ctx, p, keys)
	if len(targets) == 0 {
		klog.V(3).Infof("reload: %s/%s keys %v used by no workload that needs a restart", p.ns, p.ref, keys)
		return
	}
	r.mu.Lock()
	r.nextID++
	rec := &ReloadRecord{ID: r.nextID, Timestamp: r.deps.clock().UTC(), Namespace: p.ns, Object: p.ref.String(), Keys: keys}
	r.log = append(r.log, rec)
	if len(r.log) > reloadLogSize {
		r.log = r.log[len(r.log)-reloadLogSize:]
	}
	w := &wave{rec: rec, change: p, todo: targets}
	r.waves = append(r.waves, w)
	r.mu.Unlock()
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.String()
	}
	recordEvent(r.deps, eventsvc.Event{Type: eventsvc.Info, Severity: eventsvc.SevInfo, Namespace: p.ns, Workload: p.ref.String(),
		Reason: "ConfigChanged", Message: fmt.Sprintf("changed keys %s; reloads %s", strings.Join(keys, ", "), strings.Join(names, ", "))})
}

// affected lists the workloads in the namespace the change reloads, sorted.
func (r *Reloader) affectedWorkloads(ctx context.Context, p *pendingChange, keys []string) []workloadRef {
	var out []workloadRef
	for _, wl := range r.reloadWorkloads(ctx, p.ns) {
		use := templateRefs(wl.spec)[p.ref]
		if shouldReload(workloadRules(wl.ann), p.ref, use, keys, p.objAnn, r.cfg.On) {
			out = append(out, wl.ref)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].String() < out[b].String() })
	return out
}

// reloadable is one workload's pod template, as the reloader sees it.
type reloadable struct {
	ref    workloadRef
	spec   *corev1.PodSpec
	ann    map[string]string // workload annotations
	tmpl   map[string]string // pod template annotations
	labels map[string]string // pod template labels, for the guardrails
}

// workloads lists the reloadable workloads in ns; a failed list is counted.
func (r *Reloader) reloadWorkloads(ctx context.Context, ns string) []reloadable {
	var out []reloadable
	add := func(kind string, meta metav1.ObjectMeta, t *corev1.PodTemplateSpec) {
		out = append(out, reloadable{workloadRef{kind, meta.Name}, &t.Spec, meta.Annotations, t.Annotations, t.Labels})
	}
	c := r.deps.Client
	if l, err := c.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); err != nil {
		countAPIError(err, "deployments", ns)
	} else {
		for i := range l.Items {
			add("Deployment", l.Items[i].ObjectMeta, &l.Items[i].Spec.Template)
		}
	}
	if l, err := c.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{}); err != nil {
		countAPIError(err, "statefulsets", ns)
	} else {
		for i := range l.Items {
			add("StatefulSet", l.Items[i].ObjectMeta, &l.Items[i].Spec.Template)
		}
	}
	if l, err := c.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{}); err != nil {
		countAPIError(err, "daemonsets", ns)
	} else {
		for i := range l.Items {
			add("DaemonSet", l.Items[i].ObjectMeta, &l.Items[i].Spec.Template)
		}
	}
	if l, err := c.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{}); err != nil {
		countAPIError(err, "cronjobs", ns)
	} else {
		for i := range l.Items {
			add("CronJob", l.Items[i].ObjectMeta, &l.Items[i].Spec.JobTemplate.Spec.Template)
		}
	}
	return out
}

// advance moves a wave on: wait for the workload in flight, then restart
// the next one. A failure stops the wave (A3.1).
func (r *Reloader) advanceWave(ctx context.Context, w *wave) {
	if w.current != nil {
		done, ok, detail := r.verifyReload(ctx, w)
		switch {
		case !done:
			return
		case !ok:
			r.failWave(ctx, w, detail)
			return
		}
		r.recordOutcome(w, *w.current, "healthy", detail)
		w.current = nil
	}
	for len(w.todo) > 0 {
		next := w.todo[0]
		w.todo = w.todo[1:]
		if r.restartForReload(ctx, w, next) {
			return // wait until it is healthy before the next one
		}
	}
	r.finishWave(w)
}

// failWave records a restart that did not become healthy, blocks that
// version for that workload, skips the rest and reports it (A2.3). A
// stalled Deployment is rolled back by CheckStuckRollouts.
func (r *Reloader) failWave(ctx context.Context, w *wave, detail string) {
	cur := *w.current
	r.mu.Lock()
	r.blocked[r.blockKey(w.change.ns, cur, w.hash)] = true
	r.mu.Unlock()
	r.recordOutcome(w, cur, "failed", detail)
	for _, rest := range w.todo {
		r.recordOutcome(w, rest, "skipped", "an earlier workload in this reload failed")
	}
	w.todo, w.current = nil, nil
	r.finishWave(w)
	report(ctx, r.deps, finding{Reason: "ConfigReloadFailed", Namespace: w.change.ns, Workload: cur.String(),
		Severity: eventsvc.SevCritical, Rung: RungGuided, Subject: w.change.ref.String(),
		Summary: fmt.Sprintf("the restart for %s did not become healthy: %s", w.change.ref, detail),
		Details: []string{"Changed keys: " + strings.Join(w.rec.Keys, ", "),
			"The rest of this reload is stopped; this version will not be applied to it again"},
		Fix: fmt.Sprintf("fix or revert %s; a stalled Deployment is rolled back by the stuck rollout check", w.change.ref)})
}

// restart reloads one workload through the gate; true means the change was
// applied and the wave waits for it.
func (r *Reloader) restartForReload(ctx context.Context, w *wave, wl workloadRef) bool {
	ns, ref := w.change.ns, w.change.ref
	tmpl, ok := r.reloadTemplate(ctx, ns, wl)
	if !ok {
		r.recordOutcome(w, wl, "failed", "could not read the workload")
		return false
	}
	use := templateRefs(tmpl.spec)[ref]
	if use == nil {
		use = &refUse{AllKeys: true} // named by annotation, not used: hash every key
	}
	hash := usedHash(use, w.change.data)
	annKey := reloadAnnotation(ref)
	r.mu.Lock()
	blocked := r.blocked[r.blockKey(ns, wl, hash)]
	r.mu.Unlock()
	switch { // blocked first: a stalled version stays on the template until it is rolled back
	case blocked:
		r.recordOutcome(w, wl, "blocked", "this version stalled once and is not applied again")
		return false
	case tmpl.tmpl[annKey] == hash:
		r.recordOutcome(w, wl, "up to date", "already restarted for this version")
		return false
	}
	keys := strings.Join(w.rec.Keys, ", ")
	outcome, msg := applyMutation(ctx, r.deps, mutation{
		Namespace: ns, Workload: wl.String(), Labels: tmpl.labels, Reason: "ConfigChanged", ActionType: "config_reload",
		SuccessMsg: fmt.Sprintf("restarted %s for %s (keys %s)", wl, ref, keys),
		SuggestMsg: fmt.Sprintf("restart %s for %s (keys %s)", wl, ref, keys),
		Apply: func() error {
			patch := mergePatch(templatePatch(wl.Kind, annKey, hash))
			var err error
			switch wl.Kind {
			case "Deployment":
				_, err = r.deps.Client.AppsV1().Deployments(ns).Patch(ctx, wl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			case "StatefulSet":
				_, err = r.deps.Client.AppsV1().StatefulSets(ns).Patch(ctx, wl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			case "DaemonSet":
				_, err = r.deps.Client.AppsV1().DaemonSets(ns).Patch(ctx, wl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			default: // CronJob
				_, err = r.deps.Client.BatchV1().CronJobs(ns).Patch(ctx, wl.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			}
			return err
		},
	})
	result := map[gateOutcome]string{gateSkipped: "skipped", gateSuggested: "suggested", gateSimulated: "simulated",
		gateBlocked: "blocked", gateFailed: "failed", gateApplied: "restarted"}[outcome]
	r.recordOutcome(w, wl, result, strings.TrimSpace(msg))
	if outcome != gateApplied || wl.Kind == "CronJob" { // a CronJob's next run uses the new config
		return false
	}
	w.current, w.hash, w.startedAt = &wl, hash, r.deps.clock()
	return true
}

// templatePatch sets one pod template annotation, which rolls the workload.
func templatePatch(kind, key, value string) map[string]any {
	tmpl := map[string]any{"metadata": map[string]any{"annotations": map[string]any{key: value}}}
	if kind == "CronJob" {
		return map[string]any{"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": tmpl}}}}
	}
	return map[string]any{"spec": map[string]any{"template": tmpl}}
}

// reloadAnnotation is the pod template annotation that records which
// version of ref a workload last restarted for.
func reloadAnnotation(ref objRef) string {
	sum := sha256.Sum256([]byte(ref.String()))
	return "auto-agent.io/reload-" + hex.EncodeToString(sum[:])[:10]
}

func (r *Reloader) blockKey(ns string, wl workloadRef, hash string) string {
	return ns + "/" + wl.String() + "/" + hash
}

// template reads one workload's current pod template.
func (r *Reloader) reloadTemplate(ctx context.Context, ns string, wl workloadRef) (reloadable, bool) {
	for _, t := range r.reloadWorkloads(ctx, ns) {
		if t.ref == wl {
			return t, true
		}
	}
	return reloadable{}, false
}

// verify reports whether the workload in flight has finished rolling out
// and whether it is healthy.
func (r *Reloader) verifyReload(ctx context.Context, w *wave) (done, ok bool, detail string) {
	ns, wl := w.change.ns, *w.current
	timedOut := r.deps.clock().Sub(w.startedAt) > reloadVerifyTimeout
	switch wl.Kind {
	case "Deployment":
		d, err := r.deps.Client.AppsV1().Deployments(ns).Get(ctx, wl.Name, metav1.GetOptions{})
		if err != nil {
			countAPIError(err, "deployments", ns)
			return rolloutDone(false, timedOut, "could not read the Deployment")
		}
		if isRolloutStuck(d) {
			return true, false, "the rollout passed its progress deadline"
		}
		want := valueOr(d.Spec.Replicas, 1)
		healthy := d.Status.ObservedGeneration >= d.Generation && d.Status.UpdatedReplicas == want && d.Status.AvailableReplicas == want
		return rolloutDone(healthy, false, fmt.Sprintf("%d/%d updated and available", d.Status.AvailableReplicas, want))
	case "StatefulSet":
		s, err := r.deps.Client.AppsV1().StatefulSets(ns).Get(ctx, wl.Name, metav1.GetOptions{})
		if err != nil {
			countAPIError(err, "statefulsets", ns)
			return rolloutDone(false, timedOut, "could not read the StatefulSet")
		}
		return rolloutDone(statefulSetHealthy(s), timedOut, fmt.Sprintf("%d/%d updated and ready", s.Status.ReadyReplicas, valueOr(s.Spec.Replicas, 1)))
	default: // DaemonSet
		d, err := r.deps.Client.AppsV1().DaemonSets(ns).Get(ctx, wl.Name, metav1.GetOptions{})
		if err != nil {
			countAPIError(err, "daemonsets", ns)
			return rolloutDone(false, timedOut, "could not read the DaemonSet")
		}
		st := d.Status
		healthy := st.ObservedGeneration >= d.Generation && st.UpdatedNumberScheduled == st.DesiredNumberScheduled && st.NumberAvailable == st.DesiredNumberScheduled
		return rolloutDone(healthy, timedOut, fmt.Sprintf("%d/%d updated and available", st.NumberAvailable, st.DesiredNumberScheduled))
	}
}

func statefulSetHealthy(s *appsv1.StatefulSet) bool {
	want := valueOr(s.Spec.Replicas, 1)
	return s.Status.ObservedGeneration >= s.Generation && s.Status.UpdatedReplicas == want && s.Status.ReadyReplicas == want &&
		s.Status.CurrentRevision == s.Status.UpdateRevision
}

// rolloutDone turns health and a timeout into verify's answer.
func rolloutDone(healthy, timedOut bool, detail string) (bool, bool, string) {
	switch {
	case healthy:
		return true, true, detail
	case timedOut:
		return true, false, fmt.Sprintf("not healthy after %s (%s)", reloadVerifyTimeout, detail)
	}
	return false, false, detail
}

// outcome appends a workload's result to the wave's record.
func (r *Reloader) recordOutcome(w *wave, wl workloadRef, result, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.rec.Workloads = append(w.rec.Workloads, ReloadOutcome{Workload: wl.String(), Result: result, Detail: detail})
}

func (r *Reloader) finishWave(w *wave) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w.rec.Done = true
}

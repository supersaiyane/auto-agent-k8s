package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// Windows for the pod state checks (PLAN-002 phase 10).
const (
	stuckTerminatingAfter = 5 * time.Minute  // past the deletion deadline
	podStuckAfter         = 5 * time.Minute  // unschedulable, or volumes not mounting
	probeFailureMin       = 5                // Unhealthy events inside recentEventWindow
	recentEventWindow     = 10 * time.Minute // older events are history, not news
	readinessGateAfter    = 10 * time.Minute // running but a gate still unmet
)

var (
	volumeInMessage = regexp.MustCompile(`for volume "([^"]+)"`)
	// Three forms across Kubernetes versions: "by pod <uid>", "by <ns>/<name>", "by a pod".
	preemptedBy = regexp.MustCompile(`Preempted by (?:pod (\S+)|(\S+/\S+)|a pod) on node (\S+)`)
)

// nsPods is one allowlisted namespace's pods and events, read once per pass.
type nsPods struct {
	ns     string
	pods   []corev1.Pod
	events map[string][]corev1.Event // by pod name
}

// CheckPodStates runs the pod checks that need every pod, not only the
// pods bound to this node (ISS-043): stuck terminating (10.1), volume
// mount failures (10.3), failing probes (10.4), unschedulable (10.5,
// 10.11), preemption (10.8) and unmet readiness gates (10.12).
func CheckPodStates(ctx context.Context, deps *Deps) {
	now := deps.clock()
	byUID := map[types.UID]*corev1.Pod{}
	var all []nsPods
	for _, ns := range watchedNamespaces(ctx, deps) {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "pods", ns)
			continue
		}
		evs, err := deps.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "events", ns)
			evs = &corev1.EventList{}
		}
		d := nsPods{ns: ns, pods: pods.Items, events: map[string][]corev1.Event{}}
		for _, ev := range evs.Items {
			if ev.InvolvedObject.Kind == "Pod" {
				d.events[ev.InvolvedObject.Name] = append(d.events[ev.InvolvedObject.Name], ev)
			}
		}
		for i := range d.pods {
			byUID[d.pods[i].UID] = &d.pods[i]
		}
		all = append(all, d)
	}
	for _, d := range all {
		for i := range d.pods {
			p := &d.pods[i]
			evs := d.events[p.Name]
			checkStuckTerminating(ctx, deps, p, now)
			checkUnschedulable(ctx, deps, p, now)
			checkVolumeFailures(ctx, deps, p, evs, now)
			checkProbeFailures(ctx, deps, p, evs, now)
			checkReadinessGates(ctx, deps, p, now)
		}
		checkPreemptions(ctx, deps, d, byUID, now)
	}
}

// 10.1: a pod past its deletion deadline. The deadline in
// deletionTimestamp already includes the grace period.
func checkStuckTerminating(ctx context.Context, deps *Deps, p *corev1.Pod, now time.Time) {
	dt := p.DeletionTimestamp
	if dt == nil || now.Sub(dt.Time) < stuckTerminatingAfter {
		return
	}
	details := []string{fmt.Sprintf("Deletion was due at %s, %s ago.", dt.UTC().Format(time.RFC3339), now.Sub(dt.Time).Round(time.Second))}
	if p.Spec.NodeName != "" {
		details = append(details, fmt.Sprintf("Node `%s`: a node that is down, or whose kubelet stopped, cannot confirm the pod is gone.", p.Spec.NodeName))
	}
	if len(p.Finalizers) > 0 {
		details = append(details, fmt.Sprintf("Finalizers `%s`: the controller that owns them must remove them.", strings.Join(p.Finalizers, "`, `")))
	}
	fix := fmt.Sprintf("if the node is gone or its kubelet is down, force delete with `kubectl delete pod -n %s %s --grace-period=0 --force`", p.Namespace, p.Name)
	if isStatefulSetPod(p) {
		fix += "; this is a StatefulSet pod, so first make sure the node is really down, or two copies may run at once"
	}
	report(ctx, deps, finding{
		Reason: "PodStuckTerminating", Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name, Node: p.Spec.NodeName,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: p.Name,
		Summary: "pod is stuck terminating", Details: details, Fix: fix,
	})
}

// 10.5 and 10.11: a pod the scheduler cannot place, with each constraint
// that ruled nodes out.
func checkUnschedulable(ctx context.Context, deps *Deps, p *corev1.Pod, now time.Time) {
	if p.Spec.NodeName != "" || p.Status.Phase != corev1.PodPending || p.DeletionTimestamp != nil {
		return
	}
	c := podCondition(p, corev1.PodScheduled)
	if c == nil || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
		return
	}
	since := c.LastTransitionTime.Time
	if since.IsZero() {
		since = p.CreationTimestamp.Time
	}
	if now.Sub(since) < podStuckAfter {
		return
	}
	causes := parseSchedulingFailure(c.Message)
	details := []string{"Scheduler: " + c.Message}
	var fixes []string
	seen := map[string]bool{}
	for _, sc := range causes {
		line := fmt.Sprintf("- %s: %s", sc.Kind, sc.Text)
		if sc.Nodes > 0 {
			line = fmt.Sprintf("- %s on %d node(s): %s", sc.Kind, sc.Nodes, sc.Text)
		}
		details = append(details, line)
		if !seen[sc.Kind] {
			seen[sc.Kind] = true
			fixes = append(fixes, sc.Kind+": "+schedFix(sc.Kind))
		}
	}
	report(ctx, deps, finding{
		Reason: "Unschedulable", Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: p.Name,
		Summary: fmt.Sprintf("pod cannot be scheduled for %s", now.Sub(since).Round(time.Second)),
		Details: details, Fix: strings.Join(fixes, "; "),
	})
}

// volumeFailure is one volume the kubelet could not attach or mount.
type volumeFailure struct {
	reason string // VolumeAttachFailed or VolumeMountFailed
	volume string
	cause  string
}

// volumeFailures reads FailedAttachVolume and FailedMount events, newest
// last, keeping one entry per reason and volume.
func volumeFailures(evs []corev1.Event) []volumeFailure {
	sorted := append([]corev1.Event(nil), evs...)
	sort.SliceStable(sorted, func(i, j int) bool { return eventLastSeen(&sorted[i]).Before(eventLastSeen(&sorted[j])) })
	byKey := map[string]volumeFailure{}
	var order []string
	for _, ev := range sorted {
		var reason string
		switch ev.Reason {
		case "FailedAttachVolume":
			reason = "VolumeAttachFailed"
		case "FailedMount":
			reason = "VolumeMountFailed"
		default:
			continue
		}
		vol := "unknown"
		if m := volumeInMessage.FindStringSubmatch(ev.Message); m != nil {
			vol = m[1]
		}
		cause := ev.Message
		if _, after, ok := strings.Cut(ev.Message, " : "); ok {
			cause = after
		}
		key := reason + "/" + vol
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = volumeFailure{reason: reason, volume: vol, cause: cause}
	}
	out := make([]volumeFailure, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func volumeFix(f volumeFailure, node string) string {
	c := strings.ToLower(f.cause)
	switch {
	case strings.Contains(c, "multi-attach"):
		return "the volume is still attached to another node; wait for the old pod there to stop, or detach the volume from that node"
	case strings.Contains(c, "not found"):
		return "the referenced ConfigMap, Secret or claim does not exist in this namespace; create it or fix the name in the pod spec"
	case strings.Contains(c, "permission denied"):
		return "the volume mounted but the process cannot use it; set securityContext.fsGroup or fix ownership on the volume"
	case strings.Contains(c, "timed out") || strings.Contains(c, "deadline"):
		return fmt.Sprintf("the storage driver did not answer; check the CSI node plugin on node `%s` and the storage backend", node)
	default:
		return "run `kubectl describe pod` and check the storage driver logs on the node"
	}
}

// 10.3: a scheduled pod whose volumes do not attach or mount.
func checkVolumeFailures(ctx context.Context, deps *Deps, p *corev1.Pod, evs []corev1.Event, now time.Time) {
	if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodPending || now.Sub(p.CreationTimestamp.Time) < podStuckAfter {
		return
	}
	for _, f := range volumeFailures(evs) {
		verb := "mounted"
		if f.reason == "VolumeAttachFailed" {
			verb = "attached"
		}
		report(ctx, deps, finding{
			Reason: f.reason, Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name, Node: p.Spec.NodeName,
			Severity: eventsvc.SevCritical, Rung: RungGuided, Subject: p.Name + "/" + f.volume,
			Summary: fmt.Sprintf("volume `%s` cannot be %s", f.volume, verb),
			Details: []string{"Kubelet: " + f.cause}, Fix: volumeFix(f, p.Spec.NodeName),
		})
	}
}

// 10.4: liveness, readiness and startup probes failing, reported apart,
// before the failures turn into a crashloop.
func checkProbeFailures(ctx context.Context, deps *Deps, p *corev1.Pod, evs []corev1.Event, now time.Time) {
	if p.DeletionTimestamp != nil {
		return
	}
	type agg struct {
		kind, container, last string
		count                 int32
	}
	byKey := map[string]*agg{}
	var order []string
	for i := range evs {
		ev := &evs[i]
		if ev.Reason != "Unhealthy" || now.Sub(eventLastSeen(ev)) > recentEventWindow {
			continue
		}
		kind := probeKind(ev.Message)
		if kind == "" {
			continue
		}
		cname := containerFromFieldPath(ev.InvolvedObject.FieldPath)
		key := kind + "/" + cname
		a, ok := byKey[key]
		if !ok {
			a = &agg{kind: kind, container: cname}
			byKey[key] = a
			order = append(order, key)
		}
		a.count += eventCount(ev)
		a.last = ev.Message
	}
	for _, key := range order {
		a := byKey[key]
		if a.count < probeFailureMin || crashLooping(p, a.container) {
			continue
		}
		pr := probeOf(p, a.container, a.kind)
		details := []string{fmt.Sprintf("%d failures in the last %s. Last: %s", a.count, recentEventWindow, a.last)}
		timeout := int32(1)
		if pr != nil {
			if pr.TimeoutSeconds > 0 {
				timeout = pr.TimeoutSeconds
			}
			details = append(details, fmt.Sprintf("Probe: initialDelaySeconds=%d periodSeconds=%d timeoutSeconds=%d failureThreshold=%d",
				pr.InitialDelaySeconds, pr.PeriodSeconds, timeout, pr.FailureThreshold))
		}
		report(ctx, deps, finding{
			Reason: probeReason(a.kind), Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name, Node: p.Spec.NodeName,
			Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: p.Name + "/" + a.container,
			Summary: fmt.Sprintf("%s probe failing on container `%s`", a.kind, a.container),
			Details: details, Fix: probeFix(a.kind, timeout, hasStartupProbe(p, a.container)),
		})
	}
}

func probeKind(msg string) string {
	switch {
	case strings.HasPrefix(msg, "Liveness probe"):
		return "liveness"
	case strings.HasPrefix(msg, "Readiness probe"):
		return "readiness"
	case strings.HasPrefix(msg, "Startup probe"):
		return "startup"
	}
	return ""
}

func probeReason(kind string) string {
	switch kind {
	case "liveness":
		return "LivenessProbeFailing"
	case "readiness":
		return "ReadinessProbeFailing"
	}
	return "StartupProbeFailing"
}

func probeFix(kind string, timeout int32, hasStartup bool) string {
	switch kind {
	case "liveness":
		fix := "the kubelet restarts the container each time this probe fails its threshold"
		if !hasStartup {
			fix += "; if the app is only slow to start, add a startupProbe (or raise initialDelaySeconds)"
		}
		return fix + fmt.Sprintf("; if it answers slowly under load, raise timeoutSeconds above %ds", timeout)
	case "readiness":
		return fmt.Sprintf("the pod is taken out of Service endpoints while this fails; check what the endpoint depends on, or raise timeoutSeconds above %ds if responses are slow", timeout)
	}
	return "the container is restarted when startup does not finish within failureThreshold times periodSeconds; raise failureThreshold if a slow start is expected"
}

// 10.12: a running pod held out of Service traffic by a readiness gate.
func checkReadinessGates(ctx context.Context, deps *Deps, p *corev1.Pod, now time.Time) {
	if len(p.Spec.ReadinessGates) == 0 || p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil ||
		now.Sub(p.CreationTimestamp.Time) < readinessGateAfter {
		return
	}
	var unmet []string
	for _, g := range p.Spec.ReadinessGates {
		c := podCondition(p, g.ConditionType)
		switch {
		case c == nil:
			unmet = append(unmet, fmt.Sprintf("`%s` (never set)", g.ConditionType))
		case c.Status != corev1.ConditionTrue:
			unmet = append(unmet, fmt.Sprintf("`%s` (%s)", g.ConditionType, c.Status))
		}
	}
	if len(unmet) == 0 {
		return
	}
	report(ctx, deps, finding{
		Reason: "ReadinessGateUnmet", Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name, Node: p.Spec.NodeName,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: p.Name,
		Summary: "readiness gate not met, so the pod gets no Service traffic",
		Details: []string{"Unmet: " + strings.Join(unmet, ", ")},
		Fix:     "the controller that owns the gate (for example a load balancer controller for target health) has not set the condition; check its logs and that it can see this pod",
	})
}

// 10.8: pods the scheduler evicted to make room for higher priority pods,
// naming the victim and, when it is in the allowlist, the preemptor.
func checkPreemptions(ctx context.Context, deps *Deps, d nsPods, byUID map[types.UID]*corev1.Pod, now time.Time) {
	byName := map[string]*corev1.Pod{}
	for i := range d.pods {
		byName[d.pods[i].Name] = &d.pods[i]
	}
	for victim, evs := range d.events {
		for i := range evs {
			ev := &evs[i]
			if ev.Reason != "Preempted" || now.Sub(eventLastSeen(ev)) > recentEventWindow {
				continue
			}
			preemptor, node := "a higher priority pod", "unknown"
			if m := preemptedBy.FindStringSubmatch(ev.Message); m != nil {
				node = m[3]
				switch {
				case m[1] != "":
					preemptor = "pod with uid " + m[1] + " (outside the allowlist)"
					if pp, ok := byUID[types.UID(m[1])]; ok {
						preemptor = fmt.Sprintf("`%s/%s` (priority %s)", pp.Namespace, pp.Name, priorityOf(pp))
					}
				case m[2] != "":
					preemptor = "`" + m[2] + "`"
				}
			}
			wl := victim
			if vp, ok := byName[victim]; ok {
				wl = ownerName(vp)
			}
			report(ctx, deps, finding{
				Reason: "Preempted", Namespace: d.ns, Workload: wl, Pod: victim, Node: node,
				Severity: eventsvc.SevInfo, Rung: RungGuided, Subject: string(ev.UID),
				Summary: fmt.Sprintf("pod `%s` was preempted on node `%s` to make room for %s", victim, node, preemptor),
				Fix:     "if this workload must not be displaced, give it a higher PriorityClass; otherwise lower the preemptor's priority or set preemptionPolicy: Never on it",
			})
		}
	}
}

func priorityOf(p *corev1.Pod) string {
	if p.Spec.PriorityClassName != "" {
		return p.Spec.PriorityClassName
	}
	if p.Spec.Priority != nil {
		return fmt.Sprint(*p.Spec.Priority)
	}
	return "default"
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// eventLastSeen is the latest time an event is known to have happened,
// across the old (lastTimestamp) and new (series, eventTime) fields.
func eventLastSeen(ev *corev1.Event) time.Time {
	switch {
	case ev.Series != nil && !ev.Series.LastObservedTime.IsZero():
		return ev.Series.LastObservedTime.Time
	case !ev.LastTimestamp.IsZero():
		return ev.LastTimestamp.Time
	case !ev.EventTime.IsZero():
		return ev.EventTime.Time
	}
	return ev.CreationTimestamp.Time
}

func eventCount(ev *corev1.Event) int32 {
	if ev.Series != nil && ev.Series.Count > 0 {
		return ev.Series.Count
	}
	if ev.Count > 0 {
		return ev.Count
	}
	return 1
}

// containerFromFieldPath turns "spec.containers{app}" into "app".
func containerFromFieldPath(fp string) string {
	if i := strings.Index(fp, "{"); i >= 0 && strings.HasSuffix(fp, "}") {
		return fp[i+1 : len(fp)-1]
	}
	return "unknown"
}

func crashLooping(p *corev1.Pod, cname string) bool {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == cname && cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

func containerSpec(p *corev1.Pod, cname string) *corev1.Container {
	for i := range p.Spec.Containers {
		if p.Spec.Containers[i].Name == cname {
			return &p.Spec.Containers[i]
		}
	}
	return nil
}

func probeOf(p *corev1.Pod, cname, kind string) *corev1.Probe {
	c := containerSpec(p, cname)
	if c == nil {
		return nil
	}
	switch kind {
	case "liveness":
		return c.LivenessProbe
	case "readiness":
		return c.ReadinessProbe
	}
	return c.StartupProbe
}

func hasStartupProbe(p *corev1.Pod, cname string) bool {
	c := containerSpec(p, cname)
	return c != nil && c.StartupProbe != nil
}

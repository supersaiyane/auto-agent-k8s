package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// reloadHarness is a reloader over a fake cluster with a hand-moved clock.
type reloadHarness struct {
	*findingHarness
	r   *Reloader
	now time.Time
	kc  *fake.Clientset
}

func newReloadHarness(t *testing.T, cfg config.Reload, objs ...runtime.Object) *reloadHarness {
	t.Helper()
	h := &reloadHarness{findingHarness: newFindingHarness(t, objs...), now: testNow}
	h.kc = h.deps.Client.(*fake.Clientset)
	h.deps.Now = func() time.Time { return h.now }
	openGuardrails(h.deps)
	h.r = NewReloader(h.deps, cfg, func() bool { return true })
	return h
}

func (h *reloadHarness) advance(d time.Duration) { h.now = h.now.Add(d); h.r.reloadTick(context.Background()) }

func deployUsing(name string, spec corev1.PodSpec, mut func(*appsv1.Deployment)) *appsv1.Deployment {
	one := int32(1)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec:   appsv1.DeploymentSpec{Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}}, Spec: spec}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	if mut != nil {
		mut(d)
	}
	return d
}

func envSpec(cm, key string) corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "X", ValueFrom: cmKey(cm, key)}}}}}
}

func cmap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"}, Data: data}
}

func (h *reloadHarness) patches() []string {
	var out []string
	for _, a := range h.kc.Actions() {
		if a.GetVerb() == "patch" {
			out = append(out, a.GetResource().Resource+"/"+a.(interface{ GetName() string }).GetName())
		}
	}
	return out
}

func (h *reloadHarness) results(t *testing.T) string {
	t.Helper()
	recs := h.r.Records()
	var out []string
	for i := len(recs) - 1; i >= 0; i-- {
		for _, w := range recs[i].Workloads {
			out = append(out, w.Workload+"="+w.Result)
		}
	}
	return strings.Join(out, " ")
}

var reloadCfg = config.Reload{Enabled: true, On: "auto", Debounce: 10 * time.Second}

// A2.2: three quick edits give one restart, after the debounce window; an
// unused key changing restarts nothing.
func TestReload_DebounceAndKeyLevel(t *testing.T) {
	h := newReloadHarness(t, reloadCfg, deployUsing("api", envSpec("app", "level"), nil), deployUsing("web", envSpec("app", "color"), nil))
	v1 := cmap(map[string]string{"level": "info", "color": "blue"})
	for _, lvl := range []string{"debug", "warn", "error"} {
		v2 := cmap(map[string]string{"level": lvl, "color": "blue"})
		h.r.configMapChanged(v1, v2)
		v1 = v2
		h.advance(3 * time.Second)
	}
	if len(h.patches()) != 0 {
		t.Fatalf("nothing happens inside the debounce window: %v", h.patches())
	}
	h.advance(10 * time.Second)
	if p := h.patches(); len(p) != 1 || p[0] != "deployments/api" {
		t.Fatalf("one restart, of the workload using the changed key: %v", p)
	}
	h.advance(time.Second) // api is healthy (status unchanged), wave ends
	if got := h.results(t); got != "deployment/api=restarted deployment/api=healthy" {
		t.Fatalf("record: %s", got)
	}
	recs := h.r.Records()
	if len(recs) != 1 || strings.Join(recs[0].Keys, ",") != "level" || !recs[0].Done || recs[0].Object != "configmap/app" {
		t.Fatalf("records: %+v", recs)
	}
	d, err := h.kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil || d.Spec.Template.Annotations[reloadAnnotation(objRef{refConfigMap, "app"})] == "" {
		t.Fatalf("the restart patched the pod template annotation: %v %v", err, d.Spec.Template.Annotations)
	}

	// The same version again (a duplicate event) is up to date.
	h.r.configMapChanged(cmap(map[string]string{"level": "x", "color": "blue"}), v1)
	h.advance(11 * time.Second)
	if !strings.HasSuffix(h.results(t), "deployment/api=up to date") || len(h.patches()) != 1 {
		t.Fatalf("an applied version is not restarted twice: %s %v", h.results(t), h.patches())
	}
}

// A2.2: under reloadOn auto, a plain volume mount is updated in place and
// does not restart; under always it does.
func TestReload_ReloadOn(t *testing.T) {
	vol := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", VolumeMounts: []corev1.VolumeMount{{Name: "v", MountPath: "/etc/app"}}}},
		Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app"}}}}}}
	for _, tc := range []struct {
		on      string
		patched int
	}{{"auto", 0}, {"always", 1}} {
		cfg := reloadCfg
		cfg.On = tc.on
		h := newReloadHarness(t, cfg, deployUsing("files", vol, nil))
		h.r.configMapChanged(cmap(map[string]string{"a": "1"}), cmap(map[string]string{"a": "2"}))
		h.advance(11 * time.Second)
		if len(h.patches()) != tc.patched {
			t.Errorf("reloadOn %s: %d patches", tc.on, len(h.patches()))
		}
		if tc.on == "auto" && len(h.r.Records()) != 0 {
			t.Error("a change that restarts nothing is not recorded")
		}
	}
}

// A3.1 and A2.3: one workload at a time; the next waits until the previous
// is healthy; a stall stops the wave, blocks that version and reports it.
func TestReload_StagedWaveAndFailure(t *testing.T) {
	notReady := func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 0 }
	h := newReloadHarness(t, reloadCfg,
		deployUsing("a-first", envSpec("app", "level"), nil),
		deployUsing("b-second", envSpec("app", "level"), notReady),
		deployUsing("c-third", envSpec("app", "level"), nil))
	h.r.configMapChanged(cmap(map[string]string{"level": "1"}), cmap(map[string]string{"level": "2"}))
	h.advance(11 * time.Second)
	if p := h.patches(); len(p) != 1 || p[0] != "deployments/a-first" {
		t.Fatalf("only the first is restarted: %v", p)
	}
	h.advance(time.Second) // a-first healthy, b-second restarted
	if p := h.patches(); len(p) != 2 || p[1] != "deployments/b-second" {
		t.Fatalf("the second follows once the first is healthy: %v", p)
	}
	h.advance(time.Second)
	if len(h.patches()) != 2 {
		t.Fatal("the third waits while the second is not ready")
	}
	b, err := h.kc.AppsV1().Deployments("default").Get(context.Background(), "b-second", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	if _, err := h.kc.AppsV1().Deployments("default").UpdateStatus(context.Background(), b, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Second)
	want := "deployment/a-first=restarted deployment/a-first=healthy deployment/b-second=restarted deployment/b-second=failed deployment/c-third=skipped"
	if got := h.results(t); got != want || len(h.patches()) != 2 {
		t.Fatalf("the wave stops at the failure:\n got  %s\n want %s", got, want)
	}
	m := h.expect(t, "ConfigReloadFailed", RungGuided, 1)[0]
	if !strings.Contains(m, "deployment/b-second") || !strings.Contains(m, "progress deadline") {
		t.Fatalf("finding: %s", m)
	}
	// The same version is not pushed to b-second again.
	h.r.configMapChanged(cmap(map[string]string{"level": "x"}), cmap(map[string]string{"level": "2"}))
	h.advance(11 * time.Second)
	if !strings.Contains(h.results(t), "deployment/b-second=blocked") {
		t.Fatalf("a stalled version is blocked: %s", h.results(t))
	}
}

// A2.1: StatefulSet, DaemonSet and CronJob are patched in the right place;
// a CronJob is not waited for (its next run uses the new config).
func TestReload_OtherKinds(t *testing.T) {
	one := int32(1)
	spec := envSpec("app", "level")
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Spec: appsv1.StatefulSetSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: spec}},
		Status: appsv1.StatefulSetStatus{UpdatedReplicas: 1, ReadyReplicas: 1, CurrentRevision: "r", UpdateRevision: "r"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: spec}},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2}}
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: spec}}}}}
	h := newReloadHarness(t, reloadCfg, sts, ds, cj)
	h.r.configMapChanged(cmap(map[string]string{"level": "1"}), cmap(map[string]string{"level": "2"}))
	for i := 0; i < 5; i++ {
		h.advance(11 * time.Second)
	}
	want := "cronjob/nightly=restarted daemonset/agent=restarted daemonset/agent=healthy statefulset/db=restarted statefulset/db=healthy"
	if got := h.results(t); got != want {
		t.Fatalf("kinds:\n got  %s\n want %s", got, want)
	}
	got, err := h.kc.BatchV1().CronJobs("default").Get(context.Background(), "nightly", metav1.GetOptions{})
	if err != nil || got.Spec.JobTemplate.Spec.Template.Annotations[reloadAnnotation(objRef{refConfigMap, "app"})] == "" {
		t.Fatalf("the CronJob's job template is patched: %v", err)
	}
	if done, ok, detail := rolloutDone(false, true, "0/1 ready"); !done || ok || !strings.Contains(detail, "not healthy after") {
		t.Fatalf("a timed out rollout is done and failed: %v %v %s", done, ok, detail)
	}
	if done, _, _ := rolloutDone(false, false, ""); done {
		t.Fatal("a rollout in progress is not done")
	}
}

// A2.1: dry-run records the restart and writes nothing; a namespace outside
// the fix scope only suggests; a standby controller never acts.
func TestReload_ModesScopeAndStandby(t *testing.T) {
	dry := newReloadHarness(t, reloadCfg, deployUsing("api", envSpec("app", "level"), nil))
	dry.deps.Policy().Mode = policy.DryRun
	dry.r.configMapChanged(cmap(map[string]string{"level": "1"}), cmap(map[string]string{"level": "2"}))
	dry.advance(11 * time.Second)
	if len(dry.patches()) != 0 || dry.results(t) != "deployment/api=simulated" || len(dry.deps.DryRunLog.Recent(10)) != 1 {
		t.Fatalf("dry-run: %v %s", dry.patches(), dry.results(t))
	}

	outside := newReloadHarness(t, reloadCfg, deployUsing("api", envSpec("app", "level"), nil))
	outside.deps.Policies = policy.Static(outside.deps.Policy().WithFixOverride([]string{}))
	outside.deps.Policy().Mode = policy.Fix
	outside.r.configMapChanged(cmap(map[string]string{"level": "1"}), cmap(map[string]string{"level": "2"}))
	outside.advance(11 * time.Second)
	if len(outside.patches()) != 0 || outside.results(t) != "deployment/api=suggested" {
		t.Fatalf("outside the fix scope: %v %s", outside.patches(), outside.results(t))
	}

	standby := newReloadHarness(t, reloadCfg, deployUsing("api", envSpec("app", "level"), nil))
	standby.r.leading = func() bool { return false }
	standby.r.configMapChanged(cmap(map[string]string{"level": "1"}), cmap(map[string]string{"level": "2"}))
	standby.advance(11 * time.Second)
	if len(standby.patches()) != 0 || len(standby.r.Records()) != 0 {
		t.Fatal("a standby never acts")
	}

	unwatched := newReloadHarness(t, reloadCfg)
	c := cmap(map[string]string{"a": "1"})
	c.Namespace = "payments"
	unwatched.r.configMapChanged(c, c)
	if len(unwatched.r.pending) != 0 {
		t.Fatal("an unwatched namespace is not queued")
	}
}

// A1.2: a Secret's values never appear in a log line, a message, an event
// or the Reloads record; only key names and hashes do.
func TestReload_SecretValuesNeverLeak(t *testing.T) {
	var logs bytes.Buffer
	fs := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(fs)
	_ = fs.Set("v", "5")
	_ = fs.Set("logtostderr", "false")
	klog.SetOutput(&logs)
	defer func() { klog.SetOutput(nil); _ = fs.Set("v", "0"); _ = fs.Set("logtostderr", "true") }()

	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{{Name: "P", ValueFrom: secKey("db", "password")}}}}}
	h := newReloadHarness(t, reloadCfg, deployUsing("api", spec, nil))
	h.deps.Policy().Mode = policy.DryRun
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Data: map[string][]byte{"password": []byte("old-hunter2-value")}}
	cur := old.DeepCopy()
	cur.Data["password"] = []byte("new-hunter2-value")
	h.r.secretChanged(old, cur)
	h.advance(11 * time.Second)
	klog.Flush()
	recs, _ := json.Marshal(h.r.Records())
	var evs []string
	for _, e := range h.rec.Recent(50) {
		evs = append(evs, e.Message)
	}
	all := logs.String() + string(recs) + strings.Join(h.slack.Messages(), "") + strings.Join(evs, "")
	if !strings.Contains(string(recs), "password") {
		t.Fatalf("the key name is recorded: %s", recs)
	}
	if strings.Contains(all, "hunter2") {
		t.Fatalf("a secret value leaked:\n%s", all)
	}
}

func TestReloadScopeHelpers(t *testing.T) {
	if got := systemExclusion(policy.NamespaceSet("kube-system", "agent")); got != "metadata.namespace!=agent,metadata.namespace!=kube-system" {
		t.Fatalf("selector: %s", got)
	}
	ceiling := policy.NamespaceSet("a", "b", "unwatched")
	got, all := secretScope([]string{"c"}, false, ceiling, func(ns string) bool { return ns != "unwatched" })
	if all || strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("secret scope: %v %v", got, all)
	}
	if _, all := secretScope(nil, true, nil, nil); !all {
		t.Fatal("fix-anywhere watches every non-system namespace")
	}
}

// Start: an update through the API server reaches the reloader.
func TestReload_StartWatches(t *testing.T) {
	h := newReloadHarness(t, config.Reload{Enabled: true, Secrets: true, On: "auto"}, cmap(map[string]string{"level": "1"}),
		deployUsing("api", envSpec("app", "level"), nil),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Data: map[string][]byte{"k": []byte("v")}})
	h.deps.Now = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.r.WatchConfig(ctx)
	time.Sleep(200 * time.Millisecond)
	if _, err := h.kc.CoreV1().ConfigMaps("default").Update(ctx, cmap(map[string]string{"level": "2"}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(h.patches()) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if p := h.patches(); len(p) != 1 || p[0] != "deployments/api" {
		t.Fatalf("an update through the API server reloads the workload: %v", p)
	}
}

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) metav1.Time { return metav1.NewTime(testNow.Add(-d)) }

// findingHarness runs one detector pass against a fake cluster at testNow.
type findingHarness struct {
	deps  *Deps
	slack *mockSlackClient
	rec   *eventsvc.Recorder
}

func newFindingHarness(t *testing.T, objs ...runtime.Object) *findingHarness {
	t.Helper()
	sl, rec := &mockSlackClient{}, eventsvc.NewRecorder(100)
	return &findingHarness{slack: sl, rec: rec, deps: &Deps{
		Client:   fake.NewSimpleClientset(objs...),
		Policies: policy.Static(testPolicy()),
		Slack:    sl,
		LLM:      &mockLLMClient{},
		Dedup:    ratelimit.NewDeduplicator(5 * time.Minute),
		Sink:     &mockSink{},
		CRDStore: crd.NewStore(),
		Recorder: rec,
		Now:      func() time.Time { return testNow },
	}}
}

// expect asserts exactly n findings with this reason, each carrying the
// rung, and returns their Slack messages.
func (h *findingHarness) expect(t *testing.T, reason string, rung Rung, n int) []string {
	t.Helper()
	var evs []eventsvc.Event
	for _, e := range h.rec.Recent(100) {
		if e.Reason == reason {
			evs = append(evs, e)
		}
	}
	var msgs []string
	for _, m := range h.slack.Messages() {
		if strings.HasPrefix(m, "*"+reason+"*") {
			msgs = append(msgs, m)
		}
	}
	if len(evs) != n || len(msgs) != n {
		t.Fatalf("%s: want %d findings, got %d events and %d messages: %v", reason, n, len(evs), len(msgs), h.slack.Messages())
	}
	for i := range evs {
		if evs[i].Rung != string(rung) || !strings.Contains(msgs[i], "_Rung "+string(rung)) {
			t.Fatalf("%s: rung %s expected, event %q, message %q", reason, rung, evs[i].Rung, msgs[i])
		}
	}
	return msgs
}

func incidents(reason, ns, wl string) float64 {
	return testutil.ToFloat64(obs.IncidentsTotal.WithLabelValues(reason, ns, wl))
}

func pod(name string, mut func(*corev1.Pod)) *corev1.Pod {
	isCtrl := true
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			CreationTimestamp: ago(time.Hour),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-rs", Controller: &isCtrl}}},
		Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "app", Image: "app:v1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mut != nil {
		mut(p)
	}
	return p
}

func podEvent(name, podName, reason, msg string, count int32, last time.Duration, fieldPath string) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("ev-" + name)},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "default", Name: podName, FieldPath: fieldPath},
		Reason:         reason, Message: msg, Count: count, LastTimestamp: ago(last),
	}
}

// 10.1
func TestStuckTerminating(t *testing.T) {
	stuck := pod("stuck", func(p *corev1.Pod) {
		dt := ago(6 * time.Minute)
		p.DeletionTimestamp, p.Finalizers = &dt, []string{"example.com/cleanup"}
	})
	fresh := pod("fresh", func(p *corev1.Pod) { dt := ago(time.Minute); p.DeletionTimestamp = &dt })
	h := newFindingHarness(t, stuck, fresh)
	before := incidents("PodStuckTerminating", "default", ownerName(stuck))
	CheckPodStates(context.Background(), h.deps)
	msg := h.expect(t, "PodStuckTerminating", RungGuided, 1)[0]
	for _, want := range []string{"`default/" + ownerName(stuck) + " (pod stuck)`", "node-1", "example.com/cleanup",
		"--grace-period=0 --force", "R3 approve to fix arrives with the approval queue"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if got := incidents("PodStuckTerminating", "default", ownerName(stuck)) - before; got != 1 {
		t.Fatalf("incident counter moved by %v", got)
	}
}

// 10.5 and 10.11; ISS-043: the leader reports pods no node agent can see.
func TestUnschedulable_ReportedByLeader(t *testing.T) {
	cond := func(since time.Duration) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Spec.NodeName, p.Status.Phase = "", corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: corev1.PodReasonUnschedulable, LastTransitionTime: ago(since),
				Message: "0/3 nodes are available: 1 node(s) had untolerated taint {gpu: true}, 2 Insufficient cpu. preemption: 0/3 nodes are available: 3 No preemption victims found."}}
		}
	}
	h := newFindingHarness(t, pod("stuck", cond(10*time.Minute)), pod("young", cond(time.Minute)), pod("running", nil))
	CheckPodStates(context.Background(), h.deps)
	msg := h.expect(t, "Unschedulable", RungGuided, 1)[0]
	for _, want := range []string{"(pod stuck)", "taint on 1 node(s): had untolerated taint {gpu: true}",
		"resources on 2 node(s): Insufficient cpu", "add a matching toleration", "lower the pod's requests"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// 10.3
func TestVolumeFailures(t *testing.T) {
	pending := func(age time.Duration) func(*corev1.Pod) {
		return func(p *corev1.Pod) { p.Status.Phase, p.CreationTimestamp = corev1.PodPending, ago(age) }
	}
	h := newFindingHarness(t,
		pod("mount", pending(10*time.Minute)), pod("attach", pending(10*time.Minute)), pod("young", pending(time.Minute)),
		podEvent("e1", "mount", "FailedMount", `MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`, 4, time.Minute, ""),
		podEvent("e2", "attach", "FailedAttachVolume", `Multi-Attach error for volume "pvc-1" Volume is already exclusively attached to one node and can't be attached to another`, 2, time.Minute, ""),
		podEvent("e3", "young", "FailedMount", `MountVolume.SetUp failed for volume "x" : secret "s" not found`, 1, time.Minute, ""),
	)
	CheckPodStates(context.Background(), h.deps)
	mount := h.expect(t, "VolumeMountFailed", RungGuided, 1)[0]
	if !strings.Contains(mount, "volume `config` cannot be mounted") || !strings.Contains(mount, `configmap "app-config" not found`) ||
		!strings.Contains(mount, "does not exist in this namespace") {
		t.Errorf("mount message:\n%s", mount)
	}
	attach := h.expect(t, "VolumeAttachFailed", RungGuided, 1)[0]
	if !strings.Contains(attach, "volume `pvc-1` cannot be attached") || !strings.Contains(attach, "attached to another node") {
		t.Errorf("attach message:\n%s", attach)
	}
}

func TestVolumeFix_Causes(t *testing.T) {
	for cause, want := range map[string]string{
		"permission denied":       "fsGroup",
		"timed out waiting":       "CSI node plugin on node `n1`",
		"something else happened": "kubectl describe pod",
	} {
		if got := volumeFix(volumeFailure{cause: cause}, "n1"); !strings.Contains(got, want) {
			t.Errorf("%q: %q lacks %q", cause, got, want)
		}
	}
	if v := volumeFailures([]corev1.Event{*podEvent("e", "p", "FailedMount", "no volume named here", 1, time.Minute, "")}); v[0].volume != "unknown" {
		t.Fatalf("unnamed volume: %+v", v)
	}
}

// 10.4: liveness and readiness reported apart; quiet below the threshold,
// for old events, and once the container is crashlooping.
func TestProbeFailures(t *testing.T) {
	probed := pod("probed", func(p *corev1.Pod) {
		p.Spec.Containers[0].LivenessProbe = &corev1.Probe{TimeoutSeconds: 1, PeriodSeconds: 10, FailureThreshold: 3}
		p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{TimeoutSeconds: 2, PeriodSeconds: 5}
	})
	looping := pod("looping", func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	})
	fp := "spec.containers{app}"
	h := newFindingHarness(t, probed, looping, pod("quiet", nil), pod("old", nil),
		podEvent("l", "probed", "Unhealthy", `Liveness probe failed: Get "http://10.0.0.1:8080/healthz": context deadline exceeded`, 6, time.Minute, fp),
		podEvent("r", "probed", "Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503", 5, 2*time.Minute, fp),
		podEvent("q", "quiet", "Unhealthy", "Readiness probe failed: 503", 2, time.Minute, fp),
		podEvent("o", "old", "Unhealthy", "Liveness probe failed: timeout", 50, time.Hour, fp),
		podEvent("c", "looping", "Unhealthy", "Liveness probe failed: refused", 9, time.Minute, fp),
	)
	CheckPodStates(context.Background(), h.deps)
	live := h.expect(t, "LivenessProbeFailing", RungGuided, 1)[0]
	for _, want := range []string{"liveness probe failing on container `app`", "6 failures", "timeoutSeconds=1", "add a startupProbe", "above 1s"} {
		if !strings.Contains(live, want) {
			t.Errorf("liveness message lacks %q:\n%s", want, live)
		}
	}
	ready := h.expect(t, "ReadinessProbeFailing", RungGuided, 1)[0]
	if !strings.Contains(ready, "taken out of Service endpoints") || !strings.Contains(ready, "above 2s") {
		t.Errorf("readiness message:\n%s", ready)
	}
	if probeFix("startup", 1, false) == "" || probeReason("startup") != "StartupProbeFailing" || probeKind("other") != "" {
		t.Fatal("startup probe helpers")
	}
	if !strings.Contains(probeFix("liveness", 1, true), "under load") || strings.Contains(probeFix("liveness", 1, true), "startupProbe") {
		t.Fatal("no startupProbe advice when one exists")
	}
}

// 10.8
func TestPreemption_NamesVictimAndPreemptor(t *testing.T) {
	big := pod("big", func(p *corev1.Pod) { p.Spec.PriorityClassName = "critical" })
	h := newFindingHarness(t, big,
		podEvent("p1", "victim", "Preempted", "Preempted by pod uid-big on node node-2", 1, time.Minute, ""),
		podEvent("p2", "elsewhere", "Preempted", "Preempted by pod 0000-outside on node node-3", 1, time.Minute, ""),
		podEvent("p3", "named", "Preempted", "Preempted by kube-system/dns on node node-4", 1, time.Minute, ""),
		podEvent("p4", "history", "Preempted", "Preempted by a pod on node node-5", 1, time.Hour, ""),
	)
	CheckPodStates(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "Preempted", RungGuided, 3), "\n")
	for _, want := range []string{"pod `victim` was preempted on node `node-2` to make room for `default/big` (priority critical)",
		"uid 0000-outside (outside the allowlist)", "`kube-system/dns`"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if strings.Contains(msgs, "node-5") {
		t.Fatal("an hour old preemption is history, not news")
	}
}

// 10.12
func TestReadinessGates(t *testing.T) {
	gate := corev1.PodConditionType("target-health.example.com/tg")
	gated := func(status corev1.ConditionStatus, age time.Duration) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.CreationTimestamp = ago(age)
			p.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: gate}, {ConditionType: "other.example.com/ready"}}
			p.Status.Conditions = []corev1.PodCondition{{Type: gate, Status: status}, {Type: "other.example.com/ready", Status: corev1.ConditionTrue}}
		}
	}
	missing := pod("missing", func(p *corev1.Pod) {
		p.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: gate}}
	})
	h := newFindingHarness(t, pod("held", gated(corev1.ConditionFalse, 20*time.Minute)), missing,
		pod("met", gated(corev1.ConditionTrue, 20*time.Minute)), pod("young", gated(corev1.ConditionFalse, time.Minute)))
	CheckPodStates(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "ReadinessGateUnmet", RungGuided, 2), "\n")
	if !strings.Contains(msgs, "`target-health.example.com/tg` (False)") || !strings.Contains(msgs, "(never set)") ||
		strings.Contains(msgs, "other.example.com") {
		t.Fatalf("messages:\n%s", msgs)
	}
}

// The node-local Pending handler leaves unschedulable pods and volume
// failures to CheckPodStates, so each problem is reported once.
func TestHandlePending_LeavesPreciseCausesToTheLeader(t *testing.T) {
	unscheduled := pod("unscheduled", func(p *corev1.Pod) { p.Spec.NodeName, p.Status.Phase = "", corev1.PodPending })
	h := newFindingHarness(t, unscheduled)
	handlePending(context.Background(), h.deps, unscheduled)
	if len(h.slack.Messages()) != 0 {
		t.Fatal("unscheduled pods belong to the leader check")
	}

	mounting := pod("mounting", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })
	h = newFindingHarness(t, mounting, podEvent("m", "mounting", "FailedMount", `MountVolume.SetUp failed for volume "v" : x`, 1, time.Minute, ""))
	handlePending(context.Background(), h.deps, mounting)
	if len(h.slack.Messages()) != 0 {
		t.Fatal("volume failures belong to the leader check")
	}

	other := pod("other", func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })
	h = newFindingHarness(t, other)
	handlePending(context.Background(), h.deps, other)
	if len(h.slack.Messages()) != 1 {
		t.Fatal("any other pending pod still gets the generic report")
	}
}

func TestEventHelpers(t *testing.T) {
	series := &corev1.Event{Series: &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(testNow)}}
	if eventCount(series) != 7 || !eventLastSeen(series).Equal(testNow) {
		t.Fatal("series fields win")
	}
	bare := &corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: ago(time.Minute)}}
	if eventCount(bare) != 1 || !eventLastSeen(bare).Equal(testNow.Add(-time.Minute)) {
		t.Fatal("a bare event counts once at its creation time")
	}
	if et := (&corev1.Event{EventTime: metav1.NewMicroTime(testNow)}); !eventLastSeen(et).Equal(testNow) {
		t.Fatal("eventTime is used when lastTimestamp is empty")
	}
	if containerFromFieldPath("spec.initContainers{init}") != "init" || containerFromFieldPath("") != "unknown" {
		t.Fatal("field path parsing")
	}
	p := pod("p", func(p *corev1.Pod) { p.Spec.Priority = new(int32) })
	if priorityOf(p) != "0" || priorityOf(pod("q", nil)) != "default" || probeOf(p, "missing", "liveness") != nil {
		t.Fatal("priority and probe lookups")
	}
}

type failingSlack struct{ mockSlackClient }

func (f *failingSlack) Post(string) error { return errors.New("slack down") }

func TestReport_DedupsAndCountsSlackFailures(t *testing.T) {
	h := newFindingHarness(t)
	f := finding{Reason: "TestFinding", Namespace: "default", Workload: "wl", Severity: eventsvc.SevInfo, Rung: RungAlert, Summary: "s"}
	if !report(context.Background(), h.deps, f) || report(context.Background(), h.deps, f) {
		t.Fatal("the first report sends, a repeat inside the dedup window does not")
	}
	h.deps.Slack = &failingSlack{}
	errs := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("finding", "slack"))
	if !report(context.Background(), h.deps, finding{Reason: "TestFinding", Namespace: "default", Workload: "other", Rung: RungAlert}) {
		t.Fatal("a Slack failure does not stop the rest of the report")
	}
	if testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("finding", "slack"))-errs != 1 {
		t.Fatal("a Slack failure is counted")
	}
}

// A forbidden events read is counted and the pod checks still run.
func TestCheckPodStates_ForbiddenEventsStillChecksPods(t *testing.T) {
	stuck := pod("stuck", func(p *corev1.Pod) { dt := ago(time.Hour); p.DeletionTimestamp = &dt })
	h := newFindingHarness(t, stuck)
	h.deps.Client.(*fake.Clientset).PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("rbac"))
	})
	CheckPodStates(context.Background(), h.deps)
	h.expect(t, "PodStuckTerminating", RungGuided, 1)
	if listObjectEvents(context.Background(), h.deps.Client, "default", "stuck") != nil {
		t.Fatal("a forbidden read gives no events")
	}
}

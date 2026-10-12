package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/alertmanager"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
	"github.com/supersaiyane/auto-agent-k8s/internal/storage"
)

// dispatchPod is a pod of ReplicaSet api-7d9f (deployment "api") on node-1, created created ago,
// with one container "app" in state cs.
func dispatchPod(created time.Duration, cs corev1.ContainerStatus) *corev1.Pod {
	yes := true
	cs.Name = "app"
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f-x1", Namespace: "default", Labels: map[string]string{"pod-template-hash": "7d9f"},
			CreationTimestamp: metav1.NewTime(testNow.Add(-created)),
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-7d9f", Controller: &yes}}},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "app", Image: "api:v1",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func waiting(reason, msg string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
}

func runningSince(ago time.Duration) corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(testNow.Add(-ago))}}
}

// dispatchHarness is a finding harness with a rate limiter, so fixes can
// pass the gate.
func dispatchHarness(t *testing.T, pod *corev1.Pod) *findingHarness {
	t.Helper()
	h := newFindingHarness(t, pod)
	h.deps.Limiter = ratelimit.NewActionLimiter(100, 10*time.Minute)
	return h
}

// recordedIncidents lists the incidents recorded so far.
func recordedIncidents(h *findingHarness) []eventsvc.Event {
	var out []eventsvc.Event
	for _, e := range h.rec.Recent(100) {
		if e.Type == eventsvc.Incident {
			out = append(out, e)
		}
	}
	return out
}

func TestHandlePodUpdate_DispatchesEveryPodState(t *testing.T) {
	cases := []struct {
		name     string
		pod      func() *corev1.Pod
		reason   string // "" means nothing is reported
		severity eventsvc.Severity
		msg      string // part of the Slack message
	}{
		{name: "init container crash", reason: "InitContainerFailed", severity: eventsvc.SevWarning, msg: "migrate", pod: func() *corev1.Pod {
			p := dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Hour), Ready: true})
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "migrate", State: waiting("CrashLoopBackOff", "")}}
			return p
		}},
		{name: "crash loop", reason: "CrashLoopBackOff", severity: eventsvc.SevCritical, msg: "CrashLoopBackOff", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("CrashLoopBackOff", "back-off")})
		}},
		{name: "image pull", reason: "ImagePullBackOff", severity: eventsvc.SevCritical, msg: "ImagePull", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("ErrImagePull", "not found")})
		}},
		{name: "oom killed", reason: "OOMKilled", severity: eventsvc.SevCritical, msg: "current limit: `256Mi`", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Minute), RestartCount: 1, Ready: true,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}})
		}},
		{name: "config error", reason: "ConfigError", severity: eventsvc.SevCritical, msg: `secret "db" not found`, pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("CreateContainerConfigError", `secret "db" not found`)})
		}},
		{name: "restart storm", reason: "RestartStorm", severity: eventsvc.SevWarning, msg: "restarts: 6", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Minute), RestartCount: 6, Ready: true})
		}},
		{name: "run container error", reason: "RunContainerError", severity: eventsvc.SevCritical, msg: "entrypoint", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("RunContainerError", "exec: not found")})
		}},
		{name: "post start hook", reason: "PostStartHookError", severity: eventsvc.SevWarning, msg: "postStart", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("PostStartHookError", "")})
		}},
		{name: "image never pulled", reason: "ErrImageNeverPull", severity: eventsvc.SevWarning, msg: "imagePullPolicy", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("ErrImageNeverPull", "")})
		}},
		{name: "not ready past the window", reason: "NotReady", severity: eventsvc.SevWarning, msg: "readiness probe", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(notReadyAfter + time.Minute)})
		}},
		{name: "not ready inside the window", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(notReadyAfter - time.Minute)})
		}},
		{name: "pending past the window", reason: "Pending", severity: eventsvc.SevWarning, msg: "cluster lacks resources", pod: func() *corev1.Pod {
			p := dispatchPod(pendingAfter+time.Minute, corev1.ContainerStatus{State: waiting("ContainerCreating", "")})
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes: Insufficient cpu"}}
			return p
		}},
		{name: "pending inside the window", pod: func() *corev1.Pod {
			p := dispatchPod(pendingAfter-time.Minute, corev1.ContainerStatus{State: waiting("ContainerCreating", "")})
			p.Status.Phase = corev1.PodPending
			return p
		}},
		{name: "healthy", pod: func() *corev1.Pod {
			return dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Hour), Ready: true})
		}},
		{name: "namespace not watched", pod: func() *corev1.Pod {
			p := dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("CrashLoopBackOff", "")})
			p.Namespace = "elsewhere"
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := tc.pod()
			h := dispatchHarness(t, pod)
			handlePodUpdate(context.Background(), h.deps, pod, pod)
			handlePodUpdate(context.Background(), h.deps, pod, pod) // the same state again is deduplicated
			got := recordedIncidents(h)
			if tc.reason == "" {
				if len(got) != 0 {
					t.Fatalf("nothing should be reported, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Reason != tc.reason || got[0].Severity != tc.severity || got[0].Workload != "replicaset/api-7d9f" {
				t.Fatalf("want one %s incident (%s) on replicaset/api-7d9f, got %+v", tc.reason, tc.severity, got)
			}
			if msgs := strings.Join(h.slack.Messages(), "\n"); !strings.Contains(msgs, tc.msg) {
				t.Fatalf("Slack message should mention %q: %s", tc.msg, msgs)
			}
		})
	}
}

func TestHandlePodUpdate_NotReadyRestartsThePodInFixMode(t *testing.T) {
	pod := dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(notReadyAfter + time.Minute)})
	h := dispatchHarness(t, pod)
	handlePodUpdate(context.Background(), h.deps, pod, pod)
	if _, err := h.deps.Client.CoreV1().Pods("default").Get(context.Background(), pod.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("a pod not ready past the window is deleted in fix mode so it restarts")
	}
}

func TestHandlePodUpdate_SecondEventIsDeduplicated(t *testing.T) {
	pod := dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("CreateContainerConfigError", "missing")})
	h := dispatchHarness(t, pod)
	skipped := obs.DedupSkippedTotal.WithLabelValues("ConfigError")
	before := testutil.ToFloat64(skipped)
	handlePodUpdate(context.Background(), h.deps, pod, pod)
	handlePodUpdate(context.Background(), h.deps, pod, pod)
	if n := len(recordedIncidents(h)); n != 1 {
		t.Fatalf("the same state twice is reported once, got %d", n)
	}
	if got := testutil.ToFloat64(skipped); got != before+1 {
		t.Fatalf("the skipped event is counted: %v -> %v", before, got)
	}
}

func TestHandlePodUpdate_AlertsAlertmanager(t *testing.T) {
	var posts atomic.Int32
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer am.Close()
	pod := dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("RunContainerError", "")})
	h := dispatchHarness(t, pod)
	h.deps.AlertManager = alertmanager.New(am.URL, am.Client())
	handlePodUpdate(context.Background(), h.deps, pod, pod)
	if posts.Load() == 0 {
		t.Fatal("an incident is sent to Alertmanager when one is configured")
	}
}

func TestRunHandler_FullPoolDropsTheEvent(t *testing.T) {
	deps := &Deps{handlerSlots: make(chan struct{}, 1), inflight: &sync.WaitGroup{}}
	deps.handlerSlots <- struct{}{} // the pool is full
	ran := false
	runHandler(context.Background(), deps, func(context.Context) { ran = true })
	if ran {
		t.Fatal("with no free slot the event is dropped, not run")
	}

	<-deps.handlerSlots
	done := make(chan struct{})
	runHandler(context.Background(), deps, func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a handler runs with a deadline")
		}
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a handler with a free slot must run")
	}
	deps.inflight.Wait() // shutdown waits for it, then the slot is free again
	select {
	case deps.handlerSlots <- struct{}{}:
	default:
		t.Fatal("the slot must be released when the handler ends")
	}
}

// The fake clientset does not apply field selectors to watches, so this
// shows the informer wiring, not the node filter; the filter itself is
// checked on kind (make e2e).
func TestStartWatchers_PodEventReachesItsHandler(t *testing.T) {
	pod := dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Hour), Ready: true})
	h := dispatchHarness(t, pod)
	h.deps.NodeName = "node-1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartWatchers(ctx, h.deps)

	broken := pod.DeepCopy()
	broken.Status.ContainerStatuses[0].State = waiting("CreateContainerConfigError", "missing")
	if _, err := h.deps.Client.CoreV1().Pods("default").UpdateStatus(ctx, broken, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(recordedIncidents(h)) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	h.deps.inflight.Wait()
	if got := recordedIncidents(h); len(got) != 1 || got[0].Reason != "ConfigError" || got[0].Pod != pod.Name {
		t.Fatalf("a pod update must reach its handler through the informer, got %+v", got)
	}
}

func TestStartWatchers_NodePressureOnThisNodeIsHandled(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Annotations: map[string]string{"auto-agent.io/cordoned": "true"}},
		Spec:   corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}}}}
	h := newFindingHarness(t, node)
	h.deps.Limiter = ratelimit.NewActionLimiter(100, 10*time.Minute)
	h.deps.NodeName = "node-1"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartWatchers(ctx, h.deps)

	resolved := node.DeepCopy()
	resolved.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := h.deps.Client.CoreV1().Nodes().UpdateStatus(ctx, resolved, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	uncordoned := func() bool {
		n, err := h.deps.Client.CoreV1().Nodes().Get(ctx, "node-1", metav1.GetOptions{})
		return err == nil && !n.Spec.Unschedulable
	}
	deadline := time.Now().Add(10 * time.Second)
	for !uncordoned() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	h.deps.inflight.Wait()
	if !uncordoned() || !strings.Contains(strings.Join(h.slack.Messages(), "\n"), "NodePressure resolved") {
		t.Fatalf("pressure clearing on this node must uncordon it through the informer: %v", h.slack.Messages())
	}
}

func TestStartWatchers_WithoutANodeNameStartsAndStops(t *testing.T) {
	h := newFindingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	StartWatchers(ctx, h.deps) // must return once its context is done
	h.deps.inflight.Wait()
}

// failingSink cannot store a log bundle.
type failingSink struct{}

func (failingSink) Save(context.Context, string, *storage.Record) (string, error) {
	return "", errors.New("bucket gone")
}

// failingTicketer cannot open a ticket.
type failingTicketer struct{}

func (failingTicketer) CreateOrUpdate(context.Context, string, integrations.Ticket) (string, error) {
	return "", errors.New("tracker down")
}

// PLAN-004 phase 6 (B5): a log bundle or a ticket that cannot be stored is
// counted, and the incident is still reported.
func TestPodHandlers_StorageAndTicketFailuresAreCounted(t *testing.T) {
	for label, pod := range map[string]*corev1.Pod{
		"crashloop": dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("CrashLoopBackOff", "")}),
		"imagepull": dispatchPod(time.Hour, corev1.ContainerStatus{State: waiting("ErrImagePull", "")}),
		"oom": dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(time.Minute), Ready: true, RestartCount: 1,
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}}),
		"notready": dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(notReadyAfter + time.Minute)}),
	} {
		h := dispatchHarness(t, pod)
		h.deps.Sink = failingSink{}
		h.deps.Ticketer = failingTicketer{}
		storageFails := obs.HandlerErrorsTotal.WithLabelValues(label, "storage")
		ticketFails := obs.HandlerErrorsTotal.WithLabelValues("ticket", "create")
		sBefore, tBefore := testutil.ToFloat64(storageFails), testutil.ToFloat64(ticketFails)
		handlePodUpdate(context.Background(), h.deps, pod, pod)
		if len(recordedIncidents(h)) != 1 {
			t.Fatalf("%s: the incident is reported even when its logs cannot be stored", label)
		}
		if testutil.ToFloat64(storageFails) != sBefore+1 {
			t.Errorf("%s: a log bundle that cannot be stored must be counted", label)
		}
		if label != "notready" && label != "imagepull" && testutil.ToFloat64(ticketFails) <= tBefore {
			t.Errorf("%s: a ticket that cannot be opened must be counted", label)
		}
	}
}

func TestHandlePending_NamesTheSchedulingCause(t *testing.T) {
	for reason, want := range map[string]string{
		"0/3 nodes are available: 3 node(s) didn't match Pod's node affinity": "no nodes match scheduling constraints",
		"pod has unbound immediate persistentvolumeclaims":                    "PVC not bound",
	} {
		p := dispatchPod(pendingAfter+time.Minute, corev1.ContainerStatus{State: waiting("ContainerCreating", "")})
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: reason}}
		h := dispatchHarness(t, p)
		handlePodUpdate(context.Background(), h.deps, p, p)
		if msgs := strings.Join(h.slack.Messages(), "\n"); !strings.Contains(msgs, want) {
			t.Errorf("%q: want %q in %s", reason, want, msgs)
		}
	}
}

func TestTryFixAction_RecordsTheFixForVerification(t *testing.T) {
	pod := dispatchPod(time.Hour, corev1.ContainerStatus{State: runningSince(notReadyAfter + time.Minute)})
	h := dispatchHarness(t, pod)
	h.deps.FixTracker = NewFixTracker(10)
	handlePodUpdate(context.Background(), h.deps, pod, pod)
	if p := h.deps.FixTracker.Pending(); len(p) != 1 || p[0].Action != "delete_pod" || p[0].Reason != "NotReady" {
		t.Fatalf("an applied fix is queued for recovery verification: %+v", p)
	}
}

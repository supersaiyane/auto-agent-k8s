package kube

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
)

// mutationRecorder records every write verb the fake API server receives.
type mutationRecorder struct {
	mu      sync.Mutex
	actions []string
}

func (r *mutationRecorder) install(kc *fake.Clientset) {
	kc.PrependReactor("*", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		switch a.GetVerb() {
		case "create", "update", "patch", "delete", "delete-collection":
			r.mu.Lock()
			r.actions = append(r.actions, a.GetVerb()+" "+a.GetResource().Resource+"/"+a.GetSubresource())
			r.mu.Unlock()
		}
		return false, nil, nil
	})
}

func (r *mutationRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.actions...)
}

// openGuardrails gives the deps guardrails that allow every action.
func openGuardrails(deps *Deps) {
	deps.QuietHours = NewQuietHours("")
	deps.BlastRadius = NewBlastRadiusTracker(100, time.Hour)
	deps.Breaker = ratelimit.NewCircuitBreaker(100, time.Hour)
	deps.Limiter = ratelimit.NewActionLimiter(100, time.Hour)
	deps.DryRunLog = NewDryRunLog(50)
}

// mutatingDriver seeds the fake cluster for one remediation path and returns
// the call that, in fix mode with open guardrails, changes the cluster.
type mutatingDriver struct {
	name  string
	setup func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func()
}

func mustCreate(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

var isController = true

func seedPod(t *testing.T, ctx context.Context, kc *fake.Clientset, name, node string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-rs", Controller: &isController}}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app", Image: "api:v1"}}},
	}
	_, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	mustCreate(t, err)
	return pod
}

func seedDeployment(t *testing.T, ctx context.Context, kc *fake.Clientset, replicas int32) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}}}},
	}
	_, err := kc.AppsV1().Deployments("default").Create(ctx, d, metav1.CreateOptions{})
	mustCreate(t, err)
	return d
}

func seedNode(t *testing.T, ctx context.Context, kc *fake.Clientset, pressure, cordonedByUs bool) *corev1.Node {
	t.Helper()
	status := corev1.ConditionFalse
	if pressure {
		status = corev1.ConditionTrue
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: status}}},
	}
	if cordonedByUs {
		n.Spec.Unschedulable = true
		n.Annotations = map[string]string{"auto-agent.io/cordoned": "true"}
	}
	_, err := kc.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{})
	mustCreate(t, err)
	return n
}

// mutatingDrivers lists every remediation path that sends a write to the API
// server. TestMutationsOnlyThroughGate proves no write exists outside the
// gate; this table proves the gate holds for each path that reaches it.
var mutatingDrivers = []mutatingDriver{
	{"crashloop delete pod", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		pod := seedPod(t, ctx, kc, "api-1", "node-1")
		return func() { handleCrashLoop(ctx, deps, pod, "app") }
	}},
	{"init container delete pod", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		pod := seedPod(t, ctx, kc, "api-1", "node-1")
		return func() { handleInitContainerFailure(ctx, deps, pod, "init", "Error") }
	}},
	{"scale up", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		seedDeployment(t, ctx, kc, 2)
		deps.Metrics = &mockMetrics{cpu: 0.95}
		return func() { EvaluateAndScale(ctx, deps) }
	}},
	{"scale down", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		seedDeployment(t, ctx, kc, 3)
		deps.Metrics = &mockMetrics{cpu: 0.05}
		return func() { EvaluateAndScale(ctx, deps) }
	}},
	{"cleanup evicted pods", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gone-1", Namespace: "default"},
			Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
		_, err := kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
		mustCreate(t, err)
		return func() { CleanupEvictedPods(ctx, deps) }
	}},
	{"cleanup old failed jobs", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		mk := func(name string, age time.Duration) *batchv1.Job {
			j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
				CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
				OwnerReferences:   []metav1.OwnerReference{{Kind: "CronJob", Name: "nightly"}}},
				Status: batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: "True"}}}}
			_, err := kc.BatchV1().Jobs("default").Create(ctx, j, metav1.CreateOptions{})
			mustCreate(t, err)
			return j
		}
		mk("nightly-old", 2*time.Hour)
		current := mk("nightly-new", time.Minute)
		return func() { handleFailedJob(ctx, deps, current) }
	}},
	{"rollback stuck rollout", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		d := seedDeployment(t, ctx, kc, 2)
		d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
		for rev, img := range map[string]string{"1": "api:v1", "2": "api:v2"} {
			rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-" + rev, Namespace: "default",
				Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}},
				Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: img}}}}}}
			_, err := kc.AppsV1().ReplicaSets("default").Create(ctx, rs, metav1.CreateOptions{})
			mustCreate(t, err)
		}
		return func() { handleStuckRollout(ctx, deps, d) }
	}},
	{"node pressure cordon and evict", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		n := seedNode(t, ctx, kc, true, false)
		seedPod(t, ctx, kc, "api-1", "node-1")
		return func() { handleNodePressure(ctx, deps, n, n) }
	}},
	{"node pressure resolved uncordon", func(t *testing.T, ctx context.Context, deps *Deps, kc *fake.Clientset) func() {
		n := seedNode(t, ctx, kc, false, true)
		return func() { handleNodePressure(ctx, deps, n, n) }
	}},
}

// runDriver builds fresh deps, lets configure adjust them, seeds the
// cluster, then records the writes the driver sends.
func runDriver(t *testing.T, d mutatingDriver, configure func(*Deps)) []string {
	t.Helper()
	ctx := context.Background()
	deps, kc := newHandlerTestDeps(t)
	openGuardrails(deps)
	configure(deps)
	run := d.setup(t, ctx, deps, kc)
	rec := &mutationRecorder{}
	rec.install(kc)
	run()
	return rec.all()
}

// TestGate_FixModeOpenGuardrails_Mutates is the control: without it, every
// "no mutation" assertion below would also pass for a driver that never
// reaches its write at all.
func TestGate_FixModeOpenGuardrails_Mutates(t *testing.T) {
	for _, d := range mutatingDrivers {
		t.Run(d.name, func(t *testing.T) {
			got := runDriver(t, d, func(deps *Deps) { deps.Policy.Mode = policy.Fix })
			if len(got) == 0 {
				t.Fatal("expected at least one write in fix mode with open guardrails, got none")
			}
		})
	}
}

func TestGate_NoMutationOutsideFixOrWhenBlocked(t *testing.T) {
	states := []struct {
		name      string
		configure func(*Deps)
	}{
		{"observe", func(deps *Deps) { deps.Policy.Mode = policy.Observe }},
		{"suggest", func(deps *Deps) { deps.Policy.Mode = policy.Suggest }},
		{"dry-run", func(deps *Deps) { deps.Policy.Mode = policy.DryRun }},
		{"unknown mode", func(deps *Deps) { deps.Policy.Mode = policy.Mode("bogus") }},
		{"fix, quiet hours", func(deps *Deps) {
			deps.Policy.Mode = policy.Fix
			deps.QuietHours = NewQuietHours("00:00-00:01,00:01-00:00")
		}},
		{"fix, blast radius exhausted", func(deps *Deps) {
			deps.Policy.Mode = policy.Fix
			deps.BlastRadius = NewBlastRadiusTracker(0, time.Hour)
		}},
		{"fix, rate limit exhausted", func(deps *Deps) {
			deps.Policy.Mode = policy.Fix
			deps.Limiter = ratelimit.NewActionLimiter(0, time.Hour)
		}},
		{"fix, no rate limiter", func(deps *Deps) {
			deps.Policy.Mode = policy.Fix
			deps.Limiter = nil
		}},
	}
	for _, s := range states {
		for _, d := range mutatingDrivers {
			t.Run(s.name+"/"+d.name, func(t *testing.T) {
				if got := runDriver(t, d, s.configure); len(got) != 0 {
					t.Fatalf("expected no writes, got %v", got)
				}
			})
		}
	}
}

func TestGate_DryRunRecordsSimulation(t *testing.T) {
	for _, d := range mutatingDrivers {
		t.Run(d.name, func(t *testing.T) {
			var log *DryRunLog
			runDriver(t, d, func(deps *Deps) {
				deps.Policy.Mode = policy.DryRun
				log = deps.DryRunLog
			})
			if len(log.Recent(100)) == 0 {
				t.Fatal("expected the dry-run log to record the simulated action")
			}
		})
	}
}

func TestApplyMutation_ApplyErrorIsFailed(t *testing.T) {
	deps, _ := newHandlerTestDeps(t)
	openGuardrails(deps)
	deps.Policy.Mode = policy.Fix
	outcome, msg := applyMutation(context.Background(), deps, mutation{
		Namespace: "default", Workload: "api", ActionType: "delete_pod",
		Apply: func() error { return errors.New("forbidden") },
	})
	if outcome != gateFailed {
		t.Fatalf("expected gateFailed, got %v (%q)", outcome, msg)
	}
	if !strings.Contains(msg, "forbidden") {
		t.Errorf("expected the API error in the message, got %q", msg)
	}
}

// ISS-023: the rollback target must not depend on the order ReplicaSets are listed in.
func TestRollbackDeployment_PreviousRevisionIndependentOfListOrder(t *testing.T) {
	for _, names := range [][2]string{{"api-a", "api-b"}, {"api-b", "api-a"}} {
		t.Run(names[0]+"-is-rev2", func(t *testing.T) {
			ctx := context.Background()
			deps, kc := newHandlerTestDeps(t)
			openGuardrails(deps)
			seedDeployment(t, ctx, kc, 2)
			for i, rev := range []string{"2", "1"} {
				rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: "default",
					Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
					OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}},
					Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "api:v" + rev}}}}}}
				_, err := kc.AppsV1().ReplicaSets("default").Create(ctx, rs, metav1.CreateOptions{})
				mustCreate(t, err)
			}

			msg := rollbackDeployment(ctx, deps, "default", "api", nil)

			d, err := kc.AppsV1().Deployments("default").Get(ctx, "api", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := d.Spec.Template.Spec.Containers; len(got) != 1 || got[0].Image != "api:v1" {
				t.Fatalf("expected rollback to api:v1, got %+v (msg %q)", got, msg)
			}
			if d.Annotations["auto-agent.io/rollback-to"] != "1" {
				t.Errorf("expected rollback-to=1, got %q", d.Annotations["auto-agent.io/rollback-to"])
			}
		})
	}
}

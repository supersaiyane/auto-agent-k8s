package kube

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// PLAN-004 phase 6 (B5): the network checks on their other paths.

func refuse(res string) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource(res), "", errors.New("denied"))
	}
}

func TestCheckSandboxFailures_UnknownNode(t *testing.T) {
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "default"}, Reason: "FailedCreatePodSandBox",
		Message: "failed to set up sandbox", InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"},
		LastTimestamp: metav1.NewTime(testNow.Add(-time.Minute))}
	h := newFindingHarness(t, ev)
	CheckSandboxFailures(context.Background(), h.deps)
	got := recordedIncidents(h)
	if len(got) != 1 || got[0].Node != "unknown" || got[0].Reason != "PodSandboxFailed" {
		t.Fatalf("an event with no host is reported for node unknown: %+v", got)
	}
}

func watchingKubeSystem(t *testing.T, objs ...runtime.Object) *findingHarness {
	t.Helper()
	h := newFindingHarness(t, objs...)
	p := testPolicy()
	p.WatchNamespaces = policy.NamespaceSet("default", "kube-system")
	h.deps.Policies = policy.Static(p)
	return h
}

func TestCheckSystemNetworkPods_WaitsAndCounts(t *testing.T) {
	recent := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy-a", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-proxy"}},
		Spec: corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute))}}}}
	h := watchingKubeSystem(t, recent)
	CheckSystemNetworkPods(context.Background(), h.deps)
	if got := recordedIncidents(h); len(got) != 0 {
		t.Fatalf("a pod not ready for less than the window is not reported: %+v", got)
	}

	h = watchingKubeSystem(t)
	fakeOf(h).PrependReactor("list", "pods", refuse("pods"))
	refused := obs.APIErrorsTotal.WithLabelValues("pods", "forbidden")
	before := testutil.ToFloat64(refused)
	CheckSystemNetworkPods(context.Background(), h.deps)
	if testutil.ToFloat64(refused) != before+1 {
		t.Fatal("a refused pod list is counted once and the check stops")
	}
}

func TestCheckNetworkPolicyBlocks_ReadErrorsAreCounted(t *testing.T) {
	pol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "deny", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	for _, res := range []string{"networkpolicies", "services", "pods"} {
		h := newFindingHarness(t, pol)
		fakeOf(h).PrependReactor("list", res, refuse(res))
		refused := obs.APIErrorsTotal.WithLabelValues(res, "forbidden")
		before := testutil.ToFloat64(refused)
		CheckNetworkPolicyBlocks(context.Background(), h.deps)
		if testutil.ToFloat64(refused) != before+1 {
			t.Fatalf("a refused %s list must be counted", res)
		}
		if got := recordedIncidents(h); len(got) != 0 {
			t.Fatalf("nothing is reported from a failed read: %+v", got)
		}
	}
	h := newFindingHarness(t) // no NetworkPolicy at all: nothing else is read
	CheckNetworkPolicyBlocks(context.Background(), h.deps)
	for _, a := range fakeOf(h).Actions() {
		if a.GetResource().Resource == "services" {
			t.Fatal("with no NetworkPolicy the Services are not read")
		}
	}
}

func TestStartNetworkProbes_RunsOnTheIntervalAndStops(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Error(err)
		}
	}()
	var dials atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			if err := c.Close(); err != nil {
				return
			}
		}
	}()
	h := newFindingHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	StartNetworkProbes(ctx, h.deps, config.Probes{EgressTarget: ln.Addr().String(), Interval: 20 * time.Millisecond})
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if dials.Load() < 2 {
		t.Fatalf("the egress probe runs every interval, dialled %d times", dials.Load())
	}
	time.Sleep(60 * time.Millisecond) // let the loop see ctx is done
	after := dials.Load()
	time.Sleep(100 * time.Millisecond)
	if dials.Load() != after {
		t.Fatal("the probes stop when the context ends")
	}

	StartNetworkProbes(context.Background(), h.deps, config.Probes{Interval: time.Millisecond}) // nothing enabled: returns at once
}

// hotDeployment is a two-replica Deployment the scaler would scale up.
func hotDeployment(t *testing.T) (*Deps, *fake.Clientset) {
	t.Helper()
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}
	two := int32(2)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}}}}}
	if _, err := kc.AppsV1().Deployments("default").Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return deps, kc
}

func replicasOf(t *testing.T, kc *fake.Clientset) int32 {
	t.Helper()
	d, err := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

// ISS-103: an HPA check that fails is not "no HPA". Scaling then could fight
// an autoscaler, so the Deployment is skipped and the failure counted.
func TestEvaluateAndScale_UnknownHPAIsNotScaled(t *testing.T) {
	deps, kc := hotDeployment(t)
	kc.PrependReactor("list", "horizontalpodautoscalers", refuse("horizontalpodautoscalers"))
	refused := obs.APIErrorsTotal.WithLabelValues("horizontalpodautoscalers", "forbidden")
	before := testutil.ToFloat64(refused)
	EvaluateAndScale(context.Background(), deps)
	if got := replicasOf(t, kc); got != 2 {
		t.Fatalf("a Deployment whose HPA check failed must not be scaled, replicas %d", got)
	}
	if testutil.ToFloat64(refused) != before+1 {
		t.Fatal("the failed HPA check must be counted")
	}
}

func TestEvaluateAndScale_ReadFailuresAreCounted(t *testing.T) {
	deps, kc := hotDeployment(t)
	deps.Metrics = &mockMetrics{cpuErr: errors.New("prometheus down")}
	cpuFails := obs.HandlerErrorsTotal.WithLabelValues("scaler", "cpu")
	before := testutil.ToFloat64(cpuFails)
	EvaluateAndScale(context.Background(), deps)
	if replicasOf(t, kc) != 2 || testutil.ToFloat64(cpuFails) != before+1 {
		t.Fatal("a failed CPU query scales nothing and is counted")
	}

	deps, kc = hotDeployment(t)
	kc.PrependReactor("list", "deployments", refuse("deployments"))
	refused := obs.APIErrorsTotal.WithLabelValues("deployments", "forbidden")
	before = testutil.ToFloat64(refused)
	EvaluateAndScale(context.Background(), deps)
	if testutil.ToFloat64(refused) != before+1 {
		t.Fatal("a refused Deployment list must be counted")
	}

	deps, kc = hotDeployment(t)
	p := testPolicy()
	p.MaxReplicas = 2
	deps.Policies = policy.Static(p)
	EvaluateAndScale(context.Background(), deps)
	if got := replicasOf(t, kc); got != 2 {
		t.Fatalf("a Deployment at its maximum is not scaled, replicas %d", got)
	}
}

func TestCleanupEvictedPods_ReadFailureIsCounted(t *testing.T) {
	deps, kc := newTestDeps(t)
	kc.PrependReactor("list", "pods", refuse("pods"))
	refused := obs.APIErrorsTotal.WithLabelValues("pods", "forbidden")
	before := testutil.ToFloat64(refused)
	CleanupEvictedPods(context.Background(), deps)
	if testutil.ToFloat64(refused) < before+1 {
		t.Fatal("a refused list of failed pods must be counted")
	}
}

// Config reload finds the workloads that use a changed object; a kind that
// cannot be listed is counted and the others are still found.
func TestReloadWorkloads_RefusedKindsAreCounted(t *testing.T) {
	for _, res := range []string{"deployments", "statefulsets", "daemonsets", "cronjobs"} {
		h := newReloadHarness(t, config.Reload{Enabled: true}, deployUsing("web", envSpec("app-config", "level"), nil))
		h.kc.PrependReactor("list", res, refuse(res))
		refused := obs.APIErrorsTotal.WithLabelValues(res, "forbidden")
		before := testutil.ToFloat64(refused)
		got := h.r.reloadWorkloads(context.Background(), "default")
		if testutil.ToFloat64(refused) != before+1 {
			t.Errorf("a refused %s list must be counted", res)
		}
		if res != "deployments" && (len(got) != 1 || got[0].ref.Name != "web") {
			t.Errorf("with %s refused the Deployment is still found: %+v", res, got)
		}
	}
}

// ISS-105: the OOM memory bump reads the current limit as a Kubernetes
// quantity. Decimal units and fractions read right; a limit it cannot read
// proposes nothing rather than a patch that lowers or zeroes the limit.
func TestGenerateMemoryBumpContent_ReadsEveryQuantity(t *testing.T) {
	for limit, want := range map[string]string{
		"512Mi": `memory: "640Mi"`,  // 614 rounded up to 64
		"1.5Gi": `memory: "2048Mi"`, // 1843 rounded up to 256; was read as 1Gi
		"512M":  `memory: "640Mi"`,  // 489Mi; was read as 0
		"1G":    `memory: "1280Mi"`, // 954Mi; was read as 0
	} {
		patch, change, err := GenerateMemoryBumpContent("default", "deployment/api", "app", limit, 20)
		if err != nil || !strings.Contains(patch, want) || !strings.Contains(change, limit+" -> ") {
			t.Errorf("%s: %q %q %v, want %s", limit, patch, change, err, want)
		}
	}
	for _, bad := range []string{"", "lots", "0", "-1Gi"} {
		if _, _, err := GenerateMemoryBumpContent("default", "deployment/api", "app", bad, 20); err == nil {
			t.Errorf("%q: a limit that cannot be read or is not positive must be an error", bad)
		}
	}
	h := newFindingHarness(t)
	path, content, _, note := memoryBumpChange(context.Background(), h.deps, "default", "deployment/api", "app", "lots", 20)
	if path != "" || content != nil || !strings.Contains(note, "cannot read") {
		t.Fatalf("an unreadable limit proposes nothing and says why: %q %q %q", path, content, note)
	}
}

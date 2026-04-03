package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yourorg/auto-agent/internal/crd"
	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
)

func newHandlerTestDeps(t *testing.T) (*Deps, *fake.Clientset) {
	t.Helper()
	kc := fake.NewSimpleClientset()
	sl := &mockSlackClient{}
	return &Deps{
		Client:   kc,
		Metrics:  &mockMetrics{cpu: 0.5},
		Policy:   testPolicy(),
		Slack:    sl,
		LLM:     &mockLLMClient{},
		Dedup:    ratelimit.NewDeduplicator(5 * time.Minute),
		Limiter:  ratelimit.NewActionLimiter(100, 10*time.Minute),
		Sink:     &mockSink{},
		CRDStore: crd.NewStore(),
	}, kc
}

func TestHandleCrashLoop_DeletesPod(t *testing.T) {
	deps, kc := newHandlerTestDeps(t)
	ctx := context.Background()

	isCtrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-abc123",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "api-deploy-xyz", Controller: &isCtrl},
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "api:v1"}},
		},
	}
	kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})

	handleCrashLoop(ctx, deps, pod, "app")

	// Verify pod was deleted
	_, err := kc.CoreV1().Pods("default").Get(ctx, "api-abc123", metav1.GetOptions{})
	if err == nil {
		t.Error("expected pod to be deleted in Fix mode")
	}

	// Verify Slack was notified
	sl := deps.Slack.(*mockSlackClient)
	if len(sl.Messages()) == 0 {
		t.Error("expected Slack notification")
	}
}

func TestHandleCrashLoop_SuggestMode_NoDeletion(t *testing.T) {
	deps, kc := newHandlerTestDeps(t)
	deps.Policy.Mode = policy.Suggest
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc123", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "api:v1"}},
		},
	}
	kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})

	handleCrashLoop(ctx, deps, pod, "app")

	// Pod should still exist
	_, err := kc.CoreV1().Pods("default").Get(ctx, "api-abc123", metav1.GetOptions{})
	if err != nil {
		t.Errorf("expected pod to still exist in Suggest mode, got: %v", err)
	}
}

func TestHandleCrashLoop_RateLimited(t *testing.T) {
	deps, kc := newHandlerTestDeps(t)
	deps.Limiter = ratelimit.NewActionLimiter(0, 10*time.Minute) // limit = 0, nothing allowed
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc123", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "api:v1"}},
		},
	}
	kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})

	handleCrashLoop(ctx, deps, pod, "app")

	// Pod should still exist (rate limited)
	_, err := kc.CoreV1().Pods("default").Get(ctx, "api-abc123", metav1.GetOptions{})
	if err != nil {
		t.Errorf("expected pod to survive rate limiting, got: %v", err)
	}
}

func TestHandlePodUpdate_Dedup(t *testing.T) {
	deps, kc := newHandlerTestDeps(t)
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc123", Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:   "node-1",
			Containers: []corev1.Container{{Name: "app", Image: "api:v1"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
			},
		},
	}
	kc.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})

	// First call should process
	handlePodUpdate(ctx, deps, pod, pod)

	// Wait briefly for goroutine to start
	time.Sleep(50 * time.Millisecond)

	sl := deps.Slack.(*mockSlackClient)
	firstCount := len(sl.Messages())

	// Second call should be deduped
	handlePodUpdate(ctx, deps, pod, pod)
	time.Sleep(50 * time.Millisecond)

	if len(sl.Messages()) > firstCount {
		t.Error("expected second call to be deduplicated")
	}
}

func TestHandlePodUpdate_ExcludedAnnotation(t *testing.T) {
	deps, _ := newHandlerTestDeps(t)
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "api-abc123",
			Namespace:   "default",
			Annotations: map[string]string{"auto-agent.io/disable": "true"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
			},
		},
	}

	handlePodUpdate(ctx, deps, pod, pod)
	time.Sleep(50 * time.Millisecond)

	sl := deps.Slack.(*mockSlackClient)
	if len(sl.Messages()) > 0 {
		t.Error("expected excluded pod to be skipped")
	}
}

func TestHandlePodUpdate_DisallowedNamespace(t *testing.T) {
	deps, _ := newHandlerTestDeps(t)
	ctx := context.Background()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-abc123", Namespace: "kube-system"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "app",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
			},
		},
	}

	handlePodUpdate(ctx, deps, pod, pod)
	time.Sleep(50 * time.Millisecond)

	sl := deps.Slack.(*mockSlackClient)
	if len(sl.Messages()) > 0 {
		t.Error("expected disallowed namespace to be skipped")
	}
}

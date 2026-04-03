package kube

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/yourorg/auto-agent/internal/crd"
	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
	"github.com/yourorg/auto-agent/internal/storage"
)

// mockMetrics implements metrics.Provider for testing.
type mockMetrics struct {
	cpu    float64
	cpuErr error
	gate   float64
}

func (m *mockMetrics) AvgDeploymentCPU(_ context.Context, _ *appsv1.Deployment, _ string) (float64, error) {
	return m.cpu, m.cpuErr
}
func (m *mockMetrics) QueryInstant(_ context.Context, _ string) (float64, error) {
	return m.gate, nil
}

// mockSlack collects posted messages.
type mockSlack struct {
	messages []string
}

func (m *mockSlack) Post(text string) error {
	m.messages = append(m.messages, text)
	return nil
}
func (m *mockSlack) Postf(format string, args ...any) error {
	return m.Post(fmt.Sprintf(format, args...))
}

// mockLLM returns empty advice.
type mockLLM struct{}

func (m *mockLLM) Enabled() bool                                          { return false }
func (m *mockLLM) Diagnose(_ context.Context, _, _ string) string         { return "" }
func (m *mockLLM) DiagnoseWithFallback(_ context.Context, _, _ string) string { return "" }

// mockSink discards records.
type mockSink struct{}

func (m *mockSink) Save(_ context.Context, _ string, _ *storage.Record) (string, error) {
	return "/mock/path", nil
}

func newTestDeps(t *testing.T, objects ...metav1.Object) (*Deps, *fake.Clientset) {
	t.Helper()
	var runtimeObjects []interface{}
	for _, obj := range objects {
		runtimeObjects = append(runtimeObjects, obj)
	}

	kc := fake.NewSimpleClientset()
	return &Deps{
		Client:   kc,
		Metrics:  &mockMetrics{cpu: 0.5},
		Policy:   testPolicy(),
		Slack:    &mockSlackClient{},
		LLM:     &mockLLMClient{},
		Dedup:    ratelimit.NewDeduplicator(5 * time.Minute),
		Limiter:  ratelimit.NewActionLimiter(100, 10*time.Minute),
		Sink:     &mockSink{},
		CRDStore: crd.NewStore(),
	}, kc
}

func testPolicy() *policy.Policy {
	return &policy.Policy{
		Mode:               policy.Fix,
		HPACoexistence:     true,
		CPUThreshold:       0.8,
		ScaleWindow:        "5m",
		MaxScaleStep:       2,
		MaxActionsPer10m:   100,
		NamespaceAllow:     map[string]struct{}{"default": {}},
		ExcludedAnnotation: "auto-agent.io/disable",
		CooldownUp:         "2m",
		CooldownDown:       "10m",
		MaxReplicas:        50,
		MinReplicas:        1,
	}
}

// mockSlackClient wraps slack.Client interface for testing (thread-safe).
type mockSlackClient struct {
	mu       sync.Mutex
	messages []string
}

func (m *mockSlackClient) Post(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, text)
	return nil
}
func (m *mockSlackClient) Postf(format string, args ...any) error {
	return m.Post(fmt.Sprintf(format, args...))
}
func (m *mockSlackClient) Messages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.messages...)
}

type mockLLMClient struct{}

func (m *mockLLMClient) Enabled() bool                                              { return false }
func (m *mockLLMClient) Diagnose(_ context.Context, _, _ string) string             { return "" }
func (m *mockLLMClient) DiagnoseWithFallback(_ context.Context, _, _ string) string { return "" }

func TestEvaluateAndScale_ScaleUp(t *testing.T) {
	deps, kc := newTestDeps(t)

	// High CPU + gates open (CPU-only mode)
	deps.Metrics = &mockMetrics{cpu: 0.9}

	// Create deployment
	rep := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	_, err := kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	EvaluateAndScale(context.Background(), deps)

	// Verify scale-up happened
	updated, err := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if *updated.Spec.Replicas != 4 { // 2 + step(2)
		t.Errorf("expected 4 replicas, got %d", *updated.Spec.Replicas)
	}
	if updated.Annotations[annoLastScaleUp] == "" {
		t.Error("expected last-scale-ts annotation to be set")
	}
}

func TestEvaluateAndScale_SkipsHPA(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}

	rep := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})

	// Create HPA targeting this deployment
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api-hpa", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				Kind: "Deployment",
				Name: "api",
			},
		},
	}
	kc.AutoscalingV2().HorizontalPodAutoscalers("default").Create(context.Background(), hpa, metav1.CreateOptions{})

	EvaluateAndScale(context.Background(), deps)

	// Verify NOT scaled (HPA exists)
	updated, _ := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if *updated.Spec.Replicas != 2 {
		t.Errorf("expected replicas unchanged at 2 (HPA present), got %d", *updated.Spec.Replicas)
	}
}

func TestEvaluateAndScale_RespectsMaxReplicas(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}
	deps.Policy.MaxReplicas = 5

	rep := int32(4)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})

	EvaluateAndScale(context.Background(), deps)

	updated, _ := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if *updated.Spec.Replicas != 5 {
		t.Errorf("expected replicas capped at 5, got %d", *updated.Spec.Replicas)
	}
}

func TestEvaluateAndScale_ScaleDown(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.1}

	// Set env so gates are configured but inactive
	t.Setenv("PROM_QUEUE_DEPTH", "some_metric")

	rep := int32(5)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})

	EvaluateAndScale(context.Background(), deps)

	updated, _ := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if *updated.Spec.Replicas != 4 { // 5 - 1
		t.Errorf("expected 4 replicas after scale-down, got %d", *updated.Spec.Replicas)
	}
}

func TestEvaluateAndScale_CooldownPreventsScaling(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}

	rep := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api",
			Namespace: "default",
			Annotations: map[string]string{
				annoLastScaleUp: time.Now().UTC().Format(time.RFC3339), // just scaled
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})

	EvaluateAndScale(context.Background(), deps)

	updated, _ := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if *updated.Spec.Replicas != 2 {
		t.Errorf("expected replicas unchanged due to cooldown, got %d", *updated.Spec.Replicas)
	}
}

func TestEvaluateAndScale_LowCPU_NoScaleDown_WhenGatesActive(t *testing.T) {
	deps, kc := newTestDeps(t)
	// Low CPU but gate returns positive value (load still present)
	deps.Metrics = &mockMetrics{cpu: 0.1, gate: 5.0}

	t.Setenv("PROM_QUEUE_DEPTH", "queue_depth_total")

	rep := int32(5)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &rep,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			},
		},
	}
	kc.AppsV1().Deployments("default").Create(context.Background(), deploy, metav1.CreateOptions{})

	EvaluateAndScale(context.Background(), deps)

	updated, _ := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if *updated.Spec.Replicas != 5 {
		t.Errorf("expected no scale-down (gates active), got %d replicas", *updated.Spec.Replicas)
	}
}

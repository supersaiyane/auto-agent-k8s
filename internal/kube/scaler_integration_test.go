package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
	"github.com/supersaiyane/auto-agent-k8s/internal/storage"
)

// mockMetrics implements metrics.Provider for testing.
type mockMetrics struct {
	cpu    float64
	cpuErr error
	gate   float64
	// gateErr fails QueryInstant.
	gateErr error
	// vector answers QueryVector; nil means no Prometheus.
	vector func(q string) ([]metrics.Sample, error)
}

func (m *mockMetrics) AvgDeploymentCPU(_ context.Context, _ *appsv1.Deployment, _ string) (float64, error) {
	return m.cpu, m.cpuErr
}
func (m *mockMetrics) QueryInstant(_ context.Context, _ string) (float64, error) {
	return m.gate, m.gateErr
}
func (m *mockMetrics) QueryVector(_ context.Context, q string) ([]metrics.Sample, error) {
	if m.vector == nil {
		return nil, metrics.ErrNoPromQL
	}
	return m.vector(q)
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
		Policies: policy.Static(testPolicy()),
		Slack:    &mockSlackClient{},
		LLM:      &mockLLMClient{},
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
		WatchNamespaces:    policy.NamespaceSet("default"),
		FixNamespaces:      policy.NamespaceSet("default"),
		FixCeiling:         policy.NamespaceSet("default"),
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
	deps.Policy().MaxReplicas = 5

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

	// Gates configured but inactive
	deps.ScalingGates.QueueDepth = "some_metric"

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

	deps.ScalingGates.QueueDepth = "queue_depth_total"

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

// crdPolicy is an AutoRemediationPolicy in default selecting app=api.
func crdPolicy(name string, edit func(*crd.Policy)) crd.Policy {
	p := crd.Policy{Namespace: "default", Name: name, Selector: labels.SelectorFromSet(labels.Set{"app": "api"})}
	edit(&p)
	return p
}

func apiDeployment(t *testing.T, kc *fake.Clientset, replicas int32, annotations map[string]string) {
	t.Helper()
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default", Annotations: annotations},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}}}}}
	if _, err := kc.AppsV1().Deployments("default").Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func apiReplicas(t *testing.T, kc *fake.Clientset) int32 {
	t.Helper()
	d, err := kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return *d.Spec.Replicas
}

// ISS-037: scale.step, scale.minReplicas and scale.allowHPAOverride from the
// policy that selects the Deployment's pods.
func TestEvaluateAndScale_PolicyStepMinAndHPAOverride(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}
	deps.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) {
		p.Scale = crd.ScaleConfig{Enabled: true, MaxReplicas: 10, Step: 3, MinReplicas: 4, AllowHPAOverride: true}
	})})
	apiDeployment(t, kc, 2, nil)
	if _, err := kc.AutoscalingV2().HorizontalPodAutoscalers("default").Create(context.Background(), &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	EvaluateAndScale(context.Background(), deps)
	if got := apiReplicas(t, kc); got != 5 {
		t.Fatalf("policy step 3 with the HPA override: 2 -> %d, want 5", got)
	}

	low, kc2 := newTestDeps(t)
	low.Metrics = &mockMetrics{cpu: 0.05}
	low.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) {
		p.Scale = crd.ScaleConfig{Enabled: true, MinReplicas: 4}
	})})
	apiDeployment(t, kc2, 4, nil)
	EvaluateAndScale(context.Background(), low)
	if got := apiReplicas(t, kc2); got != 4 {
		t.Fatalf("policy minReplicas 4 is a floor, got %d", got)
	}
}

func TestScaleLimitsFor(t *testing.T) {
	pol := testPolicy()
	base := scaleLimitsFor(pol, nil)
	if base.min != pol.MinReplicas || base.max != pol.MaxReplicas || base.step != int32(pol.MaxScaleStep) || base.overrideHPA ||
		base.cooldownUp != 2*time.Minute || base.cooldownDown != 10*time.Minute {
		t.Fatalf("no policy: %+v", base)
	}
	off := crdPolicy("p", func(p *crd.Policy) { p.Scale = crd.ScaleConfig{MaxReplicas: 3, Step: 9}; p.Cooldown = "30m" })
	if l := scaleLimitsFor(pol, &off); l.max != pol.MaxReplicas || l.step != int32(pol.MaxScaleStep) || l.cooldownUp != 30*time.Minute {
		t.Fatalf("scale fields need scale.enabled, cooldown does not: %+v", l)
	}
	bad := crdPolicy("p", func(p *crd.Policy) { p.Scale = crd.ScaleConfig{Enabled: true, MaxReplicas: 3, MinReplicas: 5} })
	if l := scaleLimitsFor(pol, &bad); l.min != pol.MinReplicas || l.max != 3 {
		t.Fatalf("a minimum above the maximum is ignored: %+v", l)
	}
}

// ISS-037: safety.cooldown holds the scaler back after its last scale-up.
func TestEvaluateAndScale_PolicyCooldown(t *testing.T) {
	deps, kc := newTestDeps(t)
	deps.Metrics = &mockMetrics{cpu: 0.9}
	deps.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) { p.Cooldown = "1h" })})
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	apiDeployment(t, kc, 2, map[string]string{annoLastScaleUp: recent})
	EvaluateAndScale(context.Background(), deps)
	if got := apiReplicas(t, kc); got != 2 {
		t.Fatalf("10 minutes into a 1h policy cooldown the scaler must wait, got %d", got)
	}
}

// ISS-037: requireApproval, restartStuckPods: false and maxActionsPerHour
// refuse through the gate; ISS-081: dry-run says the same.
func TestPolicyRefusal_GateAndDryRun(t *testing.T) {
	deps, _ := newTestDeps(t)
	deps.PolicyBudget = NewPolicyBudget()
	now := testNow
	deps.Now = func() time.Time { return now }
	no := false
	deps.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) { p.RestartStuckPods = &no; p.MaxActionsPerHour = 2 })})
	apiLabels := map[string]string{"app": "api"}
	m := func(action string) mutation {
		return mutation{Namespace: "default", Workload: "deployment/api", Labels: apiLabels, Reason: "r", ActionType: action,
			Apply: func() error { return nil }}
	}
	ctx := context.Background()
	if out, msg := applyMutation(ctx, deps, m("delete_pod")); out != gateBlocked || !strings.Contains(msg, "restartStuckPods") {
		t.Fatalf("pod restarts off: %v %q", out, msg)
	}
	for i := 0; i < 2; i++ {
		if out, msg := applyMutation(ctx, deps, m("scale_up")); out != gateApplied {
			t.Fatalf("action %d within budget: %v %q", i, out, msg)
		}
	}
	if out, msg := applyMutation(ctx, deps, m("scale_up")); out != gateBlocked || !strings.Contains(msg, "2 actions per hour, 2 used") {
		t.Fatalf("third action in the hour: %v %q", out, msg)
	}
	now = now.Add(time.Hour)
	if out, _ := applyMutation(ctx, deps, m("scale_up")); out != gateApplied {
		t.Fatal("an hour later the budget is free again")
	}
	other := m("scale_up")
	other.Labels = map[string]string{"app": "web"}
	if why := policyRefusal(deps, effectivePolicy(deps, "default", other.Labels), "delete_pod"); why != "" {
		t.Fatalf("a workload no policy selects is not limited: %q", why)
	}

	deps.PolicyBudget = nil
	if why := policyRefusal(deps, effectivePolicy(deps, "default", apiLabels), "scale_up"); !strings.Contains(why, "no budget tracker") {
		t.Fatalf("no tracker fails closed: %q", why)
	}

	dry, _ := newTestDeps(t)
	dry.DryRunLog = NewDryRunLog(10)
	dry.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) { p.RequireApproval = true })})
	if msg := SimulateAction(dry, "default", "deployment/api", "", apiLabels, "r", "delete_pod", "delete the pod"); !strings.Contains(msg, "blocked by CRD policy requires manual approval") {
		t.Fatalf("dry-run must report the policy the gate would apply: %q", msg)
	}
}

// channelSlack records default and per-channel posts; failing makes every
// post fail.
type channelSlack struct {
	mockSlackClient
	cmu      sync.Mutex
	channels []string
	failing  bool
}

func (c *channelSlack) Post(text string) error {
	if c.failing {
		return fmt.Errorf("webhook gone")
	}
	return c.mockSlackClient.Post(text)
}

func (c *channelSlack) PostToChannel(channel, text string) error {
	if c.failing {
		return fmt.Errorf("webhook gone")
	}
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.channels = append(c.channels, channel+": "+text)
	return nil
}

type recTicketer struct {
	name    string
	tickets []integrations.Ticket
}

func (r *recTicketer) CreateOrUpdate(_ context.Context, _ string, t integrations.Ticket) (string, error) {
	r.tickets = append(r.tickets, t)
	return "", nil
}

// ISS-037: escalation.slackChannel and escalation.ticketing; ISS-082: a
// failed post is counted.
func TestPolicySlackChannelAndTicketing(t *testing.T) {
	deps, _ := newTestDeps(t)
	sl := &channelSlack{}
	deps.Slack = sl
	deps.CRDStore.Update("default", []crd.Policy{crdPolicy("api", func(p *crd.Policy) {
		p.SlackChannel = "#payments"
		p.Ticketing = crd.Ticketing{Provider: "github", ProjectOrRepo: "corp/payments", Assignees: []string{"alice"}, Labels: []string{"payments"}}
	})})
	api, web := map[string]string{"app": "api"}, map[string]string{"app": "web"}

	postIncident(deps, "default", api, "api broke")
	postIncident(deps, "default", web, "web broke")
	if len(sl.channels) != 1 || sl.channels[0] != "#payments: api broke" || len(sl.Messages()) != 1 || sl.Messages()[0] != "web broke" {
		t.Fatalf("channel posts %v, default posts %v", sl.channels, sl.Messages())
	}

	def, alt := &recTicketer{name: "default"}, &recTicketer{name: "github"}
	deps.Ticketer = def
	asked := ""
	deps.TicketerFor = func(provider, project string) integrations.Ticketer {
		asked = provider + " " + project
		return alt
	}
	createTicket(context.Background(), deps, "default", api, "k", "title", "body")
	createTicket(context.Background(), deps, "default", web, "k2", "title", "body")
	if asked != "github corp/payments" || len(alt.tickets) != 1 || len(def.tickets) != 1 {
		t.Fatalf("policy ticketer %q: alt %d default %d", asked, len(alt.tickets), len(def.tickets))
	}
	if got := alt.tickets[0]; strings.Join(got.Assignees, ",") != "alice" || strings.Join(got.Labels, ",") != "auto-agent,kubernetes,payments" {
		t.Fatalf("policy ticket fields: %+v", got)
	}
	deps.TicketerFor = func(string, string) integrations.Ticketer { return nil }
	createTicket(context.Background(), deps, "default", api, "k3", "title", "body")
	if len(def.tickets) != 2 {
		t.Fatal("a provider the agent cannot use falls back to the default ticketer")
	}

	sl.failing = true
	before := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("slack", "post"))
	postIncident(deps, "default", api, "x")
	postIncident(deps, "default", web, "y")
	if got := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("slack", "post")) - before; got != 2 {
		t.Fatalf("failed posts counted %v, want 2", got)
	}
}

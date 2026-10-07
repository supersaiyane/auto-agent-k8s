package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// 10.2: a namespace and a claim held by finalizers, with the finalizer and
// its owning controller named; young deletions and other namespaces quiet.
func TestStuckFinalizers_NamespaceAndPVC(t *testing.T) {
	old, young := ago(time.Hour), ago(time.Minute)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "default", DeletionTimestamp: &old},
		Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{"kubernetes"}},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceTerminating, Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: "Some content in the namespace has finalizers remaining: kubernetes.io/pvc-protection in 1 resource instances"},
			{Type: "NamespaceDeletionDiscoveryFailure", Status: corev1.ConditionFalse, Message: "all good"},
		}},
	}
	outside := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments", DeletionTimestamp: &old}}
	held := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default",
		DeletionTimestamp: &old, Finalizers: []string{"kubernetes.io/pvc-protection", "example.com/backup"}}}
	fresh := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "default",
		DeletionTimestamp: &young, Finalizers: []string{"kubernetes.io/pvc-protection"}}}
	user := pod("db-0", func(p *corev1.Pod) {
		p.Spec.Volumes = []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
	})
	h := newFindingHarness(t, ns, outside, held, fresh, user)
	CheckStuckFinalizers(context.Background(), h.deps)

	nsMsg := h.expect(t, "NamespaceStuckTerminating", RungGuided, 1)[0]
	for _, want := range []string{"terminating for 1h0m0s", "pvc-protection in 1 resource instances", "`kubernetes`: the namespace controller"} {
		if !strings.Contains(nsMsg, want) {
			t.Errorf("namespace message lacks %q:\n%s", want, nsMsg)
		}
	}
	if strings.Contains(nsMsg, "all good") {
		t.Error("only true conditions are listed")
	}
	pvcMsg := h.expect(t, "PVCStuckTerminating", RungGuided, 1)[0]
	for _, want := range []string{"`default/pvc/data`", "pvc-protection controller", "`example.com/backup`: the controller that added it", "Still mounted by: `db-0`"} {
		if !strings.Contains(pvcMsg, want) {
			t.Errorf("claim message lacks %q:\n%s", want, pvcMsg)
		}
	}
	for f, want := range map[string]string{"foregroundDeletion": "garbage collector", "external-attacher/ebs.csi.aws.com": "CSI external-attacher",
		"kubernetes.io/pv-protection": "pv-protection", "orphan": "orphans"} {
		if !strings.Contains(finalizerOwner(f), want) {
			t.Errorf("%s: %s", f, finalizerOwner(f))
		}
	}
}

// 10.6: budgets that allow no eviction for long, healthy and unhealthy
// cases told apart; budgets that allow one, or are young, stay quiet.
func TestDisruptionBudgets(t *testing.T) {
	budget := func(name string, expected, healthy, desired, allowed int32, since time.Duration) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: ago(48 * time.Hour)},
			Status: policyv1.PodDisruptionBudgetStatus{ExpectedPods: expected, CurrentHealthy: healthy, DesiredHealthy: desired,
				DisruptionsAllowed: allowed, Conditions: []metav1.Condition{{Type: policyv1.DisruptionAllowedCondition,
					Status: metav1.ConditionFalse, LastTransitionTime: ago(since)}}},
		}
	}
	h := newFindingHarness(t,
		budget("tight", 3, 3, 3, 0, time.Hour), budget("sick", 3, 1, 2, 0, 2*time.Hour),
		budget("fine", 3, 3, 2, 1, time.Hour), budget("recent", 3, 3, 3, 0, time.Minute), budget("empty", 0, 0, 0, 0, time.Hour))
	CheckDisruptionBudgets(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "PDBBlocksDisruption", RungGuided, 2), "\n")
	for _, want := range []string{"`default/pdb/tight`", "Healthy 3 of 3 expected, 3 required", "maxUnavailable: 1",
		"`default/pdb/sick`", "fix those pods first"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
}

// 10.6: our own node-pressure evictions refused by a budget are counted
// and named in the node message.
func TestNodePressure_EvictionRefusedByBudgetIsCounted(t *testing.T) {
	ctx := context.Background()
	deps, kc := newHandlerTestDeps(t)
	openGuardrails(deps)
	deps.NodeName = "node-1"
	n := seedNode(t, ctx, kc, true, false)
	seedPod(t, ctx, kc, "guarded", "node-1")
	kc.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 10)
	})
	before := testutil.ToFloat64(obs.EvictionsBlockedTotal.WithLabelValues("default"))
	handleNodePressure(ctx, deps, n, n)
	if got := testutil.ToFloat64(obs.EvictionsBlockedTotal.WithLabelValues("default")) - before; got != 1 {
		t.Fatalf("refused evictions counted: %v", got)
	}
	if !strings.Contains(strings.Join(deps.Slack.(*mockSlackClient).Messages(), "\n"), "refused by a PodDisruptionBudget") {
		t.Fatal("the node message names the refusal")
	}
}

// 10.7, ISS-045: the condition reason reaches the alert with its guidance.
func TestFailedJob_ReasonInAlert(t *testing.T) {
	limit := int32(3)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec:       batchv1.JobSpec{BackoffLimit: &limit},
		Status: batchv1.JobStatus{Failed: 4, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}},
	}
	h := newFindingHarness(t, job)
	h.deps.Client.(*fake.Clientset).PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), "", nil)
	})
	handleFailedJob(context.Background(), h.deps, job)
	msg := strings.Join(h.slack.Messages(), "\n")
	for _, want := range []string{"Reason: BackoffLimitExceeded: Job has reached the specified backoff limit", "backoffLimit (3)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("job message lacks %q:\n%s", want, msg)
		}
	}
	evs := h.rec.Recent(10)
	if len(evs) != 1 || evs[0].Reason != "JobFailed" || evs[0].Rung != string(RungGuided) {
		t.Fatalf("job event: %+v", evs)
	}
	d := int64(60)
	for reason, want := range map[string]string{"DeadlineExceeded": "activeDeadlineSeconds (60)", "PodFailurePolicy": "podFailurePolicy"} {
		if got := jobFailureFix(&batchv1.Job{Spec: batchv1.JobSpec{ActiveDeadlineSeconds: &d}}, reason); !strings.Contains(got, want) {
			t.Errorf("%s: %q", reason, got)
		}
	}
	if jobFailureFix(&batchv1.Job{}, "Other") != "" || !strings.Contains(jobFailureFix(&batchv1.Job{}, "BackoffLimitExceeded"), "(6)") ||
		!strings.Contains(jobFailureFix(&batchv1.Job{}, "DeadlineExceeded"), "its activeDeadlineSeconds") {
		t.Fatal("defaults and unknown reasons")
	}
}

func hpaFixture(name string, min, max int32, limited corev1.ConditionStatus, since time.Duration, active string) *autoscalingv2.HorizontalPodAutoscaler {
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: max,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name}},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: max, DesiredReplicas: max, Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
			{Type: autoscalingv2.ScalingLimited, Status: limited, Reason: "TooManyReplicas", LastTransitionTime: ago(since),
				Message: "the desired replica count is more than the maximum replica count"}}},
	}
	if active != "" {
		h.Status.Conditions = append(h.Status.Conditions, autoscalingv2.HorizontalPodAutoscalerCondition{
			Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: active, Message: "no metrics returned"})
	}
	return h
}

// 10.15, ISS-046: only a capped HPA is "maxed out"; a fixed-size HPA, a
// young cap and a deliberate scale to zero are quiet.
func TestHPA_StuckAtMaxOnlyWhenLimited(t *testing.T) {
	h := newFindingHarness(t,
		hpaFixture("api", 2, 10, corev1.ConditionTrue, time.Hour, ""),
		hpaFixture("pinned", 4, 4, corev1.ConditionTrue, time.Hour, ""),
		hpaFixture("calm", 2, 10, corev1.ConditionFalse, time.Hour, ""),
		hpaFixture("spike", 2, 10, corev1.ConditionTrue, time.Minute, ""),
		hpaFixture("big", 2, 48, corev1.ConditionTrue, time.Hour, ""),
		hpaFixture("blind", 2, 10, corev1.ConditionFalse, time.Hour, "FailedGetResourceMetric"),
		hpaFixture("parked", 2, 10, corev1.ConditionFalse, time.Hour, "ScalingDisabled"),
	)
	CheckHPAIssues(context.Background(), h.deps)
	maxed := strings.Join(h.expect(t, "HPAMaxedOut", RungGuided, 2), "\n")
	for _, want := range []string{"`default/deployment/api`", "from 10 to 15", "held at maxReplicas 10 for 1h0m0s",
		"R3 approve to fix arrives", "from 48 to 50 (half again, within the policy ceiling of 50)"} {
		if !strings.Contains(maxed, want) {
			t.Errorf("maxed messages lack %q:\n%s", want, maxed)
		}
	}
	failed := h.expect(t, "HPAScalingFailed", RungGuided, 1)[0]
	if !strings.Contains(failed, "FailedGetResourceMetric") {
		t.Errorf("scaling failed message:\n%s", failed)
	}
	if proposeMaxReplicas(50, 50) != 50 || proposeMaxReplicas(4, 0) != 6 {
		t.Fatal("proposal respects the ceiling; zero means no ceiling")
	}

	atCeiling := newFindingHarness(t)
	p := testPolicy()
	p.MaxReplicas = 10
	atCeiling.deps.Policies = policy.Static(p)
	checkHPA(context.Background(), atCeiling.deps, hpaFixture("edge", 1, 10, corev1.ConditionTrue, time.Hour, ""), testNow)
	if !strings.Contains(strings.Join(atCeiling.slack.Messages(), "\n"), "already at the policy ceiling of 10") {
		t.Fatal("at the ceiling the fix says so")
	}
}

// 10.16, ISS-044: unauthorized and rate-limited pulls get distinct
// messages and no retry; a network failure is still retried.
func TestImagePull_CauseDecidesRetry(t *testing.T) {
	for _, tc := range []struct {
		name, kubelet, label string
		retried              bool
		fix                  string
	}{
		{"rate limited", `failed to pull "nginx:1.27": toomanyrequests: You have reached your pull rate limit`, "registry rate limit (toomanyrequests)", false, "each retry is another pull"},
		{"unauthorized", `failed to authorize: 401 Unauthorized`, "registry refused the credentials (unauthorized)", false, "the pod uses `regcred`"},
		{"not found", `manifest unknown: manifest tagged by "v9" is not found`, "image or tag does not exist", false, "that the tag was pushed"},
		{"network", `dial tcp: lookup registry.corp.test: no such host`, "node cannot reach the registry", true, "egress from node `node-1`"},
		{"unknown", `something odd`, "not recognised from the pull error", true, "check the image name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			deps, kc := newHandlerTestDeps(t)
			openGuardrails(deps)
			rec := eventsvc.NewRecorder(10)
			deps.Recorder = rec
			p := pod("api-1", func(p *corev1.Pod) {
				p.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "regcred"}}
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: tc.kubelet}}}}
			})
			_, err := kc.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{})
			mustCreate(t, err)
			handleImagePullBackOff(ctx, deps, p, "app")

			msg := strings.Join(deps.Slack.(*mockSlackClient).Messages(), "\n")
			if !strings.Contains(msg, "Cause: "+tc.label) || !strings.Contains(msg, tc.fix) {
				t.Fatalf("message:\n%s", msg)
			}
			_, getErr := kc.CoreV1().Pods("default").Get(ctx, "api-1", metav1.GetOptions{})
			if deleted := apierrors.IsNotFound(getErr); deleted != tc.retried {
				t.Fatalf("pod deleted=%v, want retry=%v", deleted, tc.retried)
			}
			if tc.retried == strings.Contains(msg, "_No retry_") {
				t.Fatal("the message says when there is no retry")
			}
			want := RungAuto
			if !tc.retried {
				want = RungGuided
			}
			if evs := rec.Recent(1); len(evs) != 1 || evs[0].Rung != string(want) {
				t.Fatalf("event rung: %+v", evs)
			}
		})
	}
	if c := classifyPull("pull access denied: insufficient_scope"); c.kind != "unauthorized" {
		t.Fatalf("denied: %+v", c)
	}
	if got := pullFix(pullCause{kind: "unauthorized"}, pod("x", nil), "img"); !strings.Contains(got, "none are set on the pod") {
		t.Fatalf("no secrets: %s", got)
	}
}

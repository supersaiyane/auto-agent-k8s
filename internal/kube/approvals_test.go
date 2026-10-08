package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/escalation"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
)

const approver = "slack:U123ABC"

// blockSlack records Block Kit posts besides text.
type blockSlack struct {
	mockSlackClient
	bmu    sync.Mutex
	blocks [][]map[string]interface{}
}

func (b *blockSlack) PostBlocks(bl []map[string]interface{}) error {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	b.blocks = append(b.blocks, bl)
	return nil
}

func testHPA() *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 4,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"}}}
}

// approvalHarness is a finding harness with the queue on and a clock the
// test moves.
func approvalHarness(t *testing.T, approvers []string, objs ...runtime.Object) (*findingHarness, *blockSlack, *time.Time) {
	t.Helper()
	h := newFindingHarness(t, objs...)
	bs := &blockSlack{}
	now := testNow
	h.deps.Slack = bs
	h.deps.Now = func() time.Time { return now }
	h.deps.Approvals = NewApprovals(30*time.Minute, approvers)
	h.deps.Limiter = ratelimit.NewActionLimiter(10, 10*time.Minute)
	return h, bs, &now
}

func hpaFinding(deps *Deps) finding {
	return finding{Reason: "HPAMaxedOut", Namespace: "default", Workload: "deployment/web", Severity: eventsvc.SevWarning,
		Rung: RungGuided, Target: RungApprove, Subject: "web", Summary: "held at max",
		Proposal: proposeHPAMax(deps, testHPA(), "deployment/web", 6)}
}

func maxReplicas(t *testing.T, h *findingHarness) int32 {
	t.Helper()
	got, err := h.deps.Client.AutoscalingV2().HorizontalPodAutoscalers("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got.Spec.MaxReplicas
}

func onlyApproval(t *testing.T, deps *Deps) ApprovalView {
	t.Helper()
	l := ListApprovals(deps)
	if len(l) != 1 {
		t.Fatalf("want one approval, got %+v", l)
	}
	return l[0]
}

func auditHas(h *findingHarness, result, detail string) bool {
	for _, e := range h.rec.Recent(100) {
		if e.Type == eventsvc.Audit && e.Result == result && strings.Contains(e.Message, detail) {
			return true
		}
	}
	return false
}

func TestApprovals_ApproveAppliesOnceAndRecordsApprover(t *testing.T) {
	h, bs, _ := approvalHarness(t, []string{approver}, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))

	item := onlyApproval(t, h.deps)
	msgs := bs.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "approval `"+item.ID+"`") {
		t.Fatalf("finding message should name the approval: %q", msgs)
	}
	if len(bs.blocks) != 1 || !strings.Contains(fmt.Sprint(bs.blocks[0]), item.ID) {
		t.Fatalf("want one Approve/Reject block naming %s, got %v", item.ID, bs.blocks)
	}
	if maxReplicas(t, h) != 4 {
		t.Fatal("nothing may change before approval")
	}

	if _, err := ApproveFix(context.Background(), h.deps, item.ID, "slack:UNOBODY"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("an unlisted user must be refused, got %v", err)
	}
	if maxReplicas(t, h) != 4 {
		t.Fatal("a refused approval changed the cluster")
	}

	msg, err := ApproveFix(context.Background(), h.deps, item.ID, approver)
	if err != nil || !strings.Contains(msg, "raised maxReplicas") {
		t.Fatalf("approve: %q %v", msg, err)
	}
	if got := maxReplicas(t, h); got != 6 {
		t.Fatalf("maxReplicas = %d, want 6", got)
	}
	if v := onlyApproval(t, h.deps); v.State != approvalApproved || v.By != approver || v.Result != "applied" {
		t.Fatalf("view after approve: %+v", v)
	}
	if !auditHas(h, "success", "approved by "+approver) {
		t.Fatal("the audit must record who approved")
	}

	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("a replayed approval must be refused, got %v", err)
	}
}

func TestApprovals_ExpiredAndRejectedAreRefused(t *testing.T) {
	h, _, now := approvalHarness(t, []string{approver}, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	id := onlyApproval(t, h.deps).ID
	*now = now.Add(31 * time.Minute)
	if _, err := ApproveFix(context.Background(), h.deps, id, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("an expired approval must be refused, got %v", err)
	}
	if v := onlyApproval(t, h.deps); v.State != approvalExpired {
		t.Fatalf("state = %s, want expired", v.State)
	}

	h2, _, _ := approvalHarness(t, []string{approver}, testHPA())
	report(context.Background(), h2.deps, hpaFinding(h2.deps))
	id = onlyApproval(t, h2.deps).ID
	if err := RejectFix(h2.deps, id, "dashboard"); err != nil {
		t.Fatal(err)
	}
	if _, err := ApproveFix(context.Background(), h2.deps, id, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("a rejected approval must not apply, got %v", err)
	}
	if maxReplicas(t, h2) != 4 || !auditHas(h2, "rejected", "rejected by dashboard") {
		t.Fatal("reject must change nothing and be audited")
	}
	if err := RejectFix(h2.deps, "nope", "dashboard"); !errors.Is(err, ErrApprovalUnknown) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := ApproveFix(context.Background(), h2.deps, "nope", approver); !errors.Is(err, ErrApprovalUnknown) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestApprovals_GuardrailsCheckedAgainAtApproval(t *testing.T) {
	h, _, _ := approvalHarness(t, []string{approver}, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	id := onlyApproval(t, h.deps).ID
	h.deps.Limiter = nil // the rate limiter fails closed
	msg, err := ApproveFix(context.Background(), h.deps, id, approver)
	if err != nil || !strings.Contains(msg, "rate limited") {
		t.Fatalf("approve under a closed limiter: %q %v", msg, err)
	}
	if maxReplicas(t, h) != 4 || onlyApproval(t, h.deps).Result != "blocked" {
		t.Fatal("the gate must still block an approved change")
	}
}

func TestApprovals_OffOrOutOfScopeQueuesNothing(t *testing.T) {
	cases := []struct {
		name      string
		approvers []string
		mode      policy.Mode
		want      string
	}{
		{"no approvers", nil, policy.Fix, "no approvers are set"},
		{"dry-run", []string{approver}, policy.DryRun, "needs fix mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, bs, _ := approvalHarness(t, c.approvers, testHPA())
			pol := testPolicy()
			pol.Mode = c.mode
			h.deps.Policies = policy.Static(pol)
			report(context.Background(), h.deps, hpaFinding(h.deps))
			if l := ListApprovals(h.deps); len(l) != 0 {
				t.Fatalf("queued %+v", l)
			}
			if msgs := bs.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0], c.want) {
				t.Fatalf("message %q should say %q", msgs, c.want)
			}
			if _, err := ApproveFix(context.Background(), h.deps, "x", approver); err == nil {
				t.Fatal("nothing to approve")
			}
		})
	}
}

func TestApprovals_SameChangeQueuedOnce(t *testing.T) {
	h, bs, _ := approvalHarness(t, []string{approver}, testHPA())
	f := hpaFinding(h.deps)
	report(context.Background(), h.deps, f)
	h.deps.Dedup.Reset(dedupKey(f.Namespace, f.Workload+"/"+f.Subject, f.Reason))
	report(context.Background(), h.deps, f)
	if l := ListApprovals(h.deps); len(l) != 1 || len(bs.blocks) != 1 || len(bs.Messages()) != 2 {
		t.Fatalf("one change, one approval and one button set: %d items, %d posts, %d messages", len(l), len(bs.blocks), len(bs.Messages()))
	}
}

func TestApprovals_QueueBounded(t *testing.T) {
	a := NewApprovals(time.Minute, []string{approver})
	for i := 0; i < maxApprovals; i++ {
		if _, _, err := a.propose(mutation{ActionType: "x", Namespace: "default", Workload: fmt.Sprintf("w%d", i)}, "", testNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := a.propose(mutation{ActionType: "y"}, "", testNow); !errors.Is(err, errApprovalQueueFull) {
		t.Fatalf("a full queue of pending items must refuse more, got %v", err)
	}
	if _, _, err := a.propose(mutation{ActionType: "y"}, "", testNow.Add(2*time.Minute)); err != nil {
		t.Fatalf("expired items must make room: %v", err)
	}
}

func TestApprovalFixes_Changes(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", UID: "u1"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "n"}},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}}}
	paused := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Paused: true}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
	stuck := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-x", Namespace: "default"}}
	h := newFindingHarness(t, cj, paused, pvc, ds, stuck)
	ctx := context.Background()

	for _, m := range []*mutation{proposeRunNow(h.deps, cj, testNow), proposeResume(h.deps, paused),
		proposeExpandPVC(h.deps, "default", "data", 12), proposeDeleteStuck(h.deps, ds, []string{"agent-x"})} {
		if err := m.Apply(); err != nil {
			t.Fatalf("%s: %v", m.ActionType, err)
		}
	}
	jobs, err := h.deps.Client.BatchV1().Jobs("default").List(ctx, metav1.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("want one job: %v %v", jobs, err)
	}
	j := jobs.Items[0]
	if j.Name != manualJobName("nightly", testNow) || j.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" ||
		len(j.OwnerReferences) != 1 || j.OwnerReferences[0].Kind != "CronJob" || j.Labels["app"] != "n" {
		t.Fatalf("job not built from the cronjob: %+v", j.ObjectMeta)
	}
	if d, err := h.deps.Client.AppsV1().Deployments("default").Get(ctx, "web", metav1.GetOptions{}); err != nil || d.Spec.Paused {
		t.Fatalf("deployment still paused: %v", err)
	}
	c, err := h.deps.Client.CoreV1().PersistentVolumeClaims("default").Get(ctx, "data", metav1.GetOptions{})
	if err != nil || c.Spec.Resources.Requests.Storage().String() != "12Gi" {
		t.Fatalf("claim: %v %v", c.Spec.Resources.Requests, err)
	}
	if _, err := h.deps.Client.CoreV1().Pods("default").Get(ctx, "agent-x", metav1.GetOptions{}); err == nil {
		t.Fatal("stuck pod not deleted")
	}
	if err := proposeDeleteStuck(h.deps, ds, []string{"gone"}).Apply(); err == nil {
		t.Fatal("a failed delete must be returned")
	}
}

func TestManualJobNameFitsALabel(t *testing.T) {
	for _, name := range []string{"short", strings.Repeat("a", 80), strings.Repeat("b", 44) + "-x"} {
		n := manualJobName(name, testNow)
		if len(n) > 63 || !strings.HasSuffix(n, fmt.Sprintf("-manual-%d", testNow.Unix())) || strings.Contains(n, "--") {
			t.Errorf("manualJobName(%q) = %q (%d)", name, n, len(n))
		}
	}
}

func TestTemplateOwnerAndCPUPatch(t *testing.T) {
	yes := true
	ctrl := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
	}
	cases := []struct {
		pod      *corev1.Pod
		kind, nm string
		ok       bool
	}{
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pod-template-hash": "abc"}, OwnerReferences: ctrl("ReplicaSet", "web-abc")}}, "Deployment", "web", true},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: ctrl("StatefulSet", "db")}}, "StatefulSet", "db", true},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: ctrl("DaemonSet", "agent")}}, "DaemonSet", "agent", true},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: ctrl("ReplicaSet", "bare")}}, "", "", false},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{OwnerReferences: ctrl("Job", "j")}}, "", "", false},
	}
	for _, c := range cases {
		k, n, ok := templateOwner(c.pod)
		if k != c.kind || n != c.nm || ok != c.ok {
			t.Errorf("templateOwner(%v) = %s %s %v", c.pod.OwnerReferences, k, n, ok)
		}
	}

	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}},
			{Name: "side", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}}}}}}
	h := newFindingHarness(t, dep)
	pod := cases[0].pod
	pod.Namespace = "default"
	m, err := proposeCPULimit(h.deps, pod, "app", resource.NewMilliQuantity(650, resource.DecimalSI))
	if err != nil || m == nil || m.Workload != "deployment/web" {
		t.Fatalf("proposal: %+v %v", m, err)
	}
	if err := m.Apply(); err != nil {
		t.Fatal(err)
	}
	got, err := h.deps.Client.AppsV1().Deployments("default").Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cs := got.Spec.Template.Spec.Containers
	if len(cs) != 2 || cs[0].Resources.Limits.Cpu().String() != "650m" || cs[1].Resources.Limits.Cpu().String() != "100m" {
		t.Fatalf("containers after patch: %+v", cs)
	}
	if m, err := proposeCPULimit(h.deps, cases[4].pod, "app", resource.NewMilliQuantity(650, resource.DecimalSI)); m != nil || err != nil {
		t.Fatal("a Job pod has no template to patch")
	}
}

// ISS-080: a critical finding and a failed fix page the escalation chain,
// once each per dedup window; warnings do not.
func TestEscalation_CriticalFindingsAndFailedFixes(t *testing.T) {
	s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 202, `{}`) })
	defer s.Close()
	h, _, _ := approvalHarness(t, nil)
	h.deps.Escalation = escalation.NewChain(config.Escalation{PagerDutyRoutingKey: "rk"}, s.Client(time.Second))
	ctx := context.Background()

	report(ctx, h.deps, finding{Reason: "Warned", Namespace: "default", Workload: "deployment/a", Severity: eventsvc.SevWarning, Rung: RungGuided, Summary: "w"})
	crit := finding{Reason: "NodeDown", Namespace: "default", Workload: "deployment/b", Severity: eventsvc.SevCritical, Rung: RungAlert, Summary: "down"}
	report(ctx, h.deps, crit)
	report(ctx, h.deps, crit) // a repeat inside the dedup window
	fail := mutation{Namespace: "default", Workload: "deployment/c", Reason: "CrashLoopBackOff", ActionType: "delete_pod",
		Apply: func() error { return errors.New("forbidden") }}
	for i := 0; i < 2; i++ {
		if out, _ := applyMutation(ctx, h.deps, fail); out != gateFailed {
			t.Fatalf("outcome %v, want failed", out)
		}
	}
	h.deps.Escalation.Wait()
	reqs := s.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[0].Body+reqs[1].Body, "NodeDown default/deployment/b") ||
		!strings.Contains(reqs[0].Body+reqs[1].Body, "FixFailed default/deployment/c") {
		t.Fatalf("want one page for the critical finding and one for the failed fix, got %d: %+v", len(reqs), reqs)
	}
}

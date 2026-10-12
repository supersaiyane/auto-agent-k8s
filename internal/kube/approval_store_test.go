package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

const agentNS = "agent"

// storedHarness is approvalHarness with the queue stored in a ConfigMap.
// The fake clientset sets no resourceVersion, which the API server always
// does, so the store object is seeded with one unless a test brings its own.
func storedHarness(t *testing.T, objs ...runtime.Object) *findingHarness {
	t.Helper()
	seeded := false
	for _, o := range objs {
		if cm, ok := o.(*corev1.ConfigMap); ok && cm.Name == ApprovalsConfigMap {
			seeded = true
		}
	}
	if !seeded {
		objs = append(objs, storeObject(""))
	}
	h, _, _ := approvalHarness(t, []string{approver}, objs...)
	h.deps.Approvals.StoreIn(h.deps.Client, agentNS)
	return h
}

// restart replaces the queue with a fresh one on the same store, as a new
// leader or a restarted controller would have.
func restart(h *findingHarness) {
	h.deps.Approvals = NewApprovals(30*time.Minute, []string{approver}).StoreIn(h.deps.Client, agentNS)
}

// stored reads the queue straight from the ConfigMap.
func stored(t *testing.T, h *findingHarness) []approvalRecord {
	t.Helper()
	cm, err := h.deps.Client.CoreV1().ConfigMaps(agentNS).Get(context.Background(), ApprovalsConfigMap, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("approval ConfigMap: %v", err)
	}
	var recs []approvalRecord
	if err := json.Unmarshal([]byte(cm.Data[approvalsKey]), &recs); err != nil {
		t.Fatalf("stored queue: %v", err)
	}
	return recs
}

// storeObject is the approval ConfigMap holding raw, with a resourceVersion.
func storeObject(raw string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ApprovalsConfigMap, Namespace: agentNS, ResourceVersion: "1"},
		Data: map[string]string{approvalsKey: raw}}
}

func fakeOf(h *findingHarness) *fake.Clientset { return h.deps.Client.(*fake.Clientset) }

func TestApprovals_SurviveARestart(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	recs := stored(t, h)
	if len(recs) != 1 || recs[0].ID != item.ID || recs[0].State != approvalPending || recs[0].Fix.Action != "raise_hpa_max" || recs[0].Fix.To != 6 {
		t.Fatalf("the pending approval must be stored with its change: %+v", recs)
	}

	restart(h)
	after := onlyApproval(t, h.deps)
	if after.ID != item.ID || after.State != approvalPending || after.Change != item.Change || after.Workload != item.Workload {
		t.Fatalf("after a restart the approval must be the same: before %+v after %+v", item, after)
	}
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); err != nil {
		t.Fatalf("approve after restart: %v", err)
	}
	if got := maxReplicas(t, h); got != 6 {
		t.Fatalf("maxReplicas = %d, want 6", got)
	}
	if r := stored(t, h)[0]; r.State != approvalApproved || r.By != approver || r.Result != "applied" {
		t.Fatalf("stored after approve: %+v", r)
	}

	restart(h)
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("an approval decided before a restart must stay decided, got %v", err)
	}
}

func TestApprovals_TwoControllersShareOneQueue(t *testing.T) {
	h := storedHarness(t, testHPA())
	first := h.deps.Approvals
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	restart(h) // the second controller takes over
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); err != nil {
		t.Fatalf("second controller approve: %v", err)
	}

	h.deps.Approvals = first // the first comes back as leader with an old copy in memory
	if v := onlyApproval(t, h.deps); v.State != approvalApproved {
		t.Fatalf("the old leader must read the stored state, got %+v", v)
	}
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("an approval decided on another controller must be refused, got %v", err)
	}
	if got := maxReplicas(t, h); got != 6 {
		t.Fatalf("maxReplicas = %d, want 6: applied once", got)
	}
}

func TestApprovals_WriteAheadNeverAppliesTwice(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	// The approved state is stored, then the controller dies before the gate.
	a := h.deps.Approvals
	a.mu.Lock()
	err := a.updateQueue(h.deps, func() (bool, error) {
		a.items[item.ID].state, a.items[item.ID].by = approvalApproved, approver
		return true, nil
	})
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	restart(h)
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("an approval stored as approved must never be applied again, got %v", err)
	}
	if got := maxReplicas(t, h); got != 4 {
		t.Fatalf("maxReplicas = %d, want 4: nothing applied", got)
	}
	if v := onlyApproval(t, h.deps); v.Result != "" {
		t.Fatalf("an interrupted approval shows no result, got %q", v.Result)
	}
}

func TestApprovals_FailedStoreWriteAppliesNothing(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	fakeOf(h).PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("denied"))
	})
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); err == nil {
		t.Fatal("approve must fail when the approved state cannot be stored")
	}
	if got := maxReplicas(t, h); got != 4 {
		t.Fatalf("maxReplicas = %d, want 4: a change that could not be recorded must not be applied", got)
	}
	if err := RejectFix(h.deps, item.ID, approver); err == nil {
		t.Fatal("reject must fail when it cannot be stored")
	}
}

func TestApprovals_QueueingFailsWhenItCannotBeStored(t *testing.T) {
	h, _, _ := approvalHarness(t, []string{approver}, testHPA())
	h.deps.Approvals.StoreIn(h.deps.Client, agentNS) // no stored queue yet: the first write creates it
	fakeOf(h).PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("denied"))
	})
	p, _, note := queueApproval(h.deps, hpaFinding(h.deps))
	if p != nil || !strings.Contains(note, "could not queue") {
		t.Fatalf("an approval that cannot be stored must not be queued: %v %q", p, note)
	}
	if _, _, err := h.deps.Approvals.propose(h.deps, mutation{ActionType: "x", Namespace: "default"}, "", testNow); !errors.Is(err, errApprovalStore) {
		t.Fatalf("a change with no fixSpec cannot be stored, got %v", err)
	}
}

func TestApprovals_ConflictIsRetriedWithTheReadVersion(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	conflicts, sawVersion := 0, false
	fakeOf(h).PrependReactor("patch", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sawVersion = strings.Contains(string(a.(k8stesting.PatchAction).GetPatch()), `"resourceVersion"`)
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("changed"))
		}
		return false, nil, nil
	})
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); err != nil {
		t.Fatalf("a conflict must be retried: %v", err)
	}
	if !sawVersion {
		t.Fatal("the store patch must carry the read resourceVersion as a precondition")
	}
	if got := maxReplicas(t, h); got != 6 {
		t.Fatalf("maxReplicas = %d, want 6", got)
	}
}

func TestApprovals_UnreadableStoredChangeIsShownNeverApplied(t *testing.T) {
	recs := []approvalRecord{{ID: "bad1", Fix: fixSpec{Action: "drop_database", Namespace: "default", Name: "db"},
		Created: testNow, Expires: testNow.Add(time.Hour), State: approvalPending}}
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	h := storedHarness(t, storeObject(string(raw)))
	v := onlyApproval(t, h.deps)
	if v.ID != "bad1" || !strings.Contains(v.Change, "unreadable stored change") {
		t.Fatalf("an unreadable change must be listed as such: %+v", v)
	}
	if _, err := ApproveFix(context.Background(), h.deps, "bad1", approver); !errors.Is(err, errApprovalStore) || !errors.Is(err, errUnknownFix) {
		t.Fatalf("an unreadable change must never be applied, got %v", err)
	}
}

func TestApprovals_CorruptStoreIsAnError(t *testing.T) {
	h := storedHarness(t, storeObject("{not json"), testHPA())
	if _, _, err := h.deps.Approvals.propose(h.deps, *hpaFinding(h.deps).Proposal, "", testNow); !errors.Is(err, errApprovalStore) {
		t.Fatalf("a corrupt store must refuse to queue, got %v", err)
	}
	if l := ListApprovals(h.deps); len(l) != 0 {
		t.Fatalf("a corrupt store lists nothing, got %+v", l)
	}
}

func TestApprovalStore_SizeBounded(t *testing.T) {
	s := configMapStore{kc: fake.NewSimpleClientset(), ns: agentNS}
	big := []approvalRecord{{ID: "x", Summary: strings.Repeat("a", maxStoredBytes)}}
	if err := s.writeApprovals(context.Background(), big, ""); !errors.Is(err, errApprovalQueueFull) {
		t.Fatalf("a queue over the size limit must be refused, got %v", err)
	}
}

func TestWriteAgentSetting_OnlyTheAgentsOwnConfigMaps(t *testing.T) {
	ran := 0
	for _, name := range []string{ApprovalsConfigMap, "auto-agent-scope"} {
		if err := writeAgentSetting(name, func() error { ran++; return nil }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := writeAgentSetting("kube-proxy", func() error { ran++; return nil }); !errors.Is(err, errNotAgentSetting) {
		t.Fatalf("any other name must be refused, got %v", err)
	}
	if ran != 2 {
		t.Fatalf("a refused write must not run, ran %d", ran)
	}
}

func TestBuildFix_EveryActionSurvivesStorage(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	paused := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
	yes := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc-1", Namespace: "default", Labels: map[string]string{"pod-template-hash": "abc"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc", Controller: &yes}}}}
	h := newFindingHarness(t)
	cpu := resource.MustParse("750m")
	cpuFix, err := proposeCPULimit(h.deps, pod, "app", &cpu)
	if err != nil || cpuFix == nil {
		t.Fatalf("cpu proposal: %v %v", cpuFix, err)
	}
	for _, m := range []*mutation{proposeHPAMax(h.deps, testHPA(), "deployment/web", 6), proposeResume(h.deps, paused),
		proposeExpandPVC(h.deps, "default", "data", 12), proposeDeleteStuck(h.deps, ds, []string{"agent-x"}),
		proposeRunNow(h.deps, cj, testNow), cpuFix} {
		raw, err := json.Marshal(m.Fix)
		if err != nil {
			t.Fatal(err)
		}
		var back fixSpec
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		got, err := buildFix(h.deps, back)
		if err != nil {
			t.Fatalf("%s: %v", m.ActionType, err)
		}
		if got.ActionType != m.ActionType || got.Namespace != m.Namespace || got.Workload != m.Workload || got.Reason != m.Reason ||
			got.SuggestMsg != m.SuggestMsg || got.SuccessMsg != m.SuccessMsg || got.Apply == nil {
			t.Fatalf("%s changed in storage:\nbefore %+v\nafter  %+v", m.ActionType, m, got)
		}
	}
	if _, err := buildFix(h.deps, fixSpec{Action: "nope"}); !errors.Is(err, errUnknownFix) {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err := buildFix(h.deps, fixSpec{Action: "raise_cpu_limit", Kind: "Job", Name: "x", Container: "c", CPU: "1"}); !errors.Is(err, errUnknownFix) {
		t.Fatalf("cpu limit on a kind with no template: %v", err)
	}
}

func TestRunNow_UsesTheCronJobAsItIsAtApproval(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default", UID: "u1"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"v": "1"}}}}}
	h := newFindingHarness(t, cj)
	m := proposeRunNow(h.deps, cj, testNow)

	ctx := context.Background()
	newer := cj.DeepCopy()
	newer.Spec.JobTemplate.Labels = map[string]string{"v": "2"}
	if _, err := h.deps.Client.BatchV1().CronJobs("default").Update(ctx, newer, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(); err != nil {
		t.Fatal(err)
	}
	j, err := h.deps.Client.BatchV1().Jobs("default").Get(ctx, manualJobName("nightly", testNow), metav1.GetOptions{})
	if err != nil || j.Labels["v"] != "2" {
		t.Fatalf("the job must use the template at approval time: %v %v", j, err)
	}
	if err := h.deps.Client.BatchV1().CronJobs("default").Delete(ctx, "nightly", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := proposeRunNow(h.deps, cj, testNow.Add(time.Minute)).Apply(); err == nil {
		t.Fatal("a CronJob gone by approval time must be an error")
	}
}

func TestSameChange_RunNowIgnoresTheJobName(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"}}
	h := newFindingHarness(t)
	a, b := proposeRunNow(h.deps, cj, testNow), proposeRunNow(h.deps, cj, testNow.Add(time.Hour))
	if !sameChange(*a, *b) {
		t.Fatal("two run-now proposals for one CronJob are the same change")
	}
	if sameChange(*proposeResume(h.deps, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}}),
		*proposeResume(h.deps, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default"}})) {
		t.Fatal("different workloads are different changes")
	}
}

func TestApprovalStore_FirstWriteCreatesAndAnObjectWithNoVersionIsRefused(t *testing.T) {
	kc := fake.NewSimpleClientset()
	s := configMapStore{kc: kc, ns: agentNS}
	ctx := context.Background()
	if recs, v, err := s.readApprovals(ctx); err != nil || v != "" || recs != nil {
		t.Fatalf("no stored queue reads as empty: %v %q %v", recs, v, err)
	}
	if err := s.writeApprovals(ctx, []approvalRecord{{ID: "a"}}, ""); err != nil {
		t.Fatalf("first write creates the ConfigMap: %v", err)
	}
	cm, err := kc.CoreV1().ConfigMaps(agentNS).Get(ctx, ApprovalsConfigMap, metav1.GetOptions{})
	if err != nil || cm.Labels["app.kubernetes.io/managed-by"] != "auto-agent" || !strings.Contains(cm.Data[approvalsKey], `"id":"a"`) {
		t.Fatalf("created ConfigMap: %+v %v", cm, err)
	}
	if err := s.writeApprovals(ctx, nil, ""); !apierrors.IsConflict(err) {
		t.Fatalf("a second create must be a conflict so the caller rereads, got %v", err)
	}
	// The fake sets no resourceVersion; the API server always does.
	if _, _, err := s.readApprovals(ctx); !errors.Is(err, errApprovalStore) {
		t.Fatalf("a stored queue with no resourceVersion must be refused, got %v", err)
	}
}

func TestApprovals_ResultWriteFailureIsCountedNotLost(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	patches := 0
	fakeOf(h).PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		patches++
		if patches > 1 { // the approved state is stored; storing the result fails
			return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("denied"))
		}
		return false, nil, nil
	})
	before := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("approvals", "store"))
	msg, err := ApproveFix(context.Background(), h.deps, item.ID, approver)
	if err != nil || !strings.Contains(msg, "raised maxReplicas") {
		t.Fatalf("the change was applied, so approve succeeds: %q %v", msg, err)
	}
	if got := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("approvals", "store")); got != before+1 {
		t.Fatalf("a failed result write must be counted: %v -> %v", before, got)
	}
	if r := stored(t, h)[0]; r.State != approvalApproved || r.Result != "" {
		t.Fatalf("the stored item stays approved without a result: %+v", r)
	}
}

func TestApprovals_ListShowsTheLastCopyWhenTheStoreIsUnreadable(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)

	fakeOf(h).PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	before := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("approvals", "store"))
	if v := onlyApproval(t, h.deps); v.ID != item.ID {
		t.Fatalf("list must show the last copy read: %+v", v)
	}
	if got := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("approvals", "store")); got != before+1 {
		t.Fatalf("an unreadable store must be counted: %v -> %v", before, got)
	}
}

func TestApprovals_RejectIsStored(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	item := onlyApproval(t, h.deps)
	if err := RejectFix(h.deps, item.ID, approver); err != nil {
		t.Fatal(err)
	}
	restart(h)
	if v := onlyApproval(t, h.deps); v.State != approvalRejected || v.By != approver {
		t.Fatalf("a rejection must survive a restart: %+v", v)
	}
	if _, err := ApproveFix(context.Background(), h.deps, item.ID, approver); !errors.Is(err, ErrApprovalDecided) {
		t.Fatalf("a rejected change cannot be approved after a restart, got %v", err)
	}
}

func TestJobFromCronJob_CopiesTemplateAnnotations(t *testing.T) {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"team": "data"}}}}}
	j := jobFromCronJob(cj, "nightly-manual-1")
	if j.Annotations["team"] != "data" || j.Annotations["cronjob.kubernetes.io/instantiate"] != "manual" {
		t.Fatalf("annotations: %v", j.Annotations)
	}
}

func TestMustFix_PanicsOnAnUnknownAction(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a propose function naming an unknown action is a programming error")
		}
	}()
	mustFix(newFindingHarness(t).deps, fixSpec{Action: "nope"})
}

func TestApprovalStore_SizeLimitIsInclusive(t *testing.T) {
	s := configMapStore{kc: fake.NewSimpleClientset(), ns: agentNS}
	empty, err := json.Marshal([]approvalRecord{{ID: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	fits := []approvalRecord{{ID: "x", Summary: strings.Repeat("a", maxStoredBytes-len(empty))}}
	if err := s.writeApprovals(context.Background(), fits, ""); err != nil {
		t.Fatalf("a queue of exactly maxStoredBytes must be stored: %v", err)
	}
	over := []approvalRecord{{ID: "x", Summary: strings.Repeat("a", maxStoredBytes-len(empty)+1)}}
	if err := s.writeApprovals(context.Background(), over, "1"); !errors.Is(err, errApprovalQueueFull) {
		t.Fatalf("one byte over the limit must be refused, got %v", err)
	}
}

func TestApprovalStore_FailedWritesCountedConflictsNot(t *testing.T) {
	kc := fake.NewSimpleClientset(storeObject(""))
	fail := apierrors.NewForbidden(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("denied"))
	kc.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, fail })
	s := configMapStore{kc: kc, ns: agentNS}
	forbidden := obs.APIErrorsTotal.WithLabelValues("configmaps", "forbidden")
	before := testutil.ToFloat64(forbidden)
	if err := s.writeApprovals(context.Background(), nil, "1"); !apierrors.IsForbidden(err) {
		t.Fatalf("got %v", err)
	}
	if got := testutil.ToFloat64(forbidden); got != before+1 {
		t.Fatalf("a refused write must be counted: %v -> %v", before, got)
	}

	fail = apierrors.NewConflict(corev1.Resource("configmaps"), ApprovalsConfigMap, errors.New("changed"))
	other := obs.APIErrorsTotal.WithLabelValues("configmaps", "other")
	before = testutil.ToFloat64(other)
	if err := s.writeApprovals(context.Background(), nil, "1"); !apierrors.IsConflict(err) {
		t.Fatalf("got %v", err)
	}
	if got := testutil.ToFloat64(other); got != before {
		t.Fatalf("a conflict is retried, not an error to count: %v -> %v", before, got)
	}
}

func TestStoreIn_NeedsAClientAndANamespace(t *testing.T) {
	kc := fake.NewSimpleClientset()
	if a := NewApprovals(time.Minute, nil).StoreIn(nil, agentNS); a.store != nil {
		t.Fatal("no client: memory only")
	}
	if a := NewApprovals(time.Minute, nil).StoreIn(kc, ""); a.store != nil {
		t.Fatal("no namespace: memory only")
	}
	if a := NewApprovals(time.Minute, nil).StoreIn(kc, agentNS); a.store == nil {
		t.Fatal("a client and a namespace make the queue durable")
	}
	var none *Approvals
	if none.StoreIn(kc, agentNS) != nil {
		t.Fatal("a nil queue stays nil")
	}
}

func TestApprovals_AStepThatChangesNothingWritesNothing(t *testing.T) {
	h := storedHarness(t, testHPA())
	report(context.Background(), h.deps, hpaFinding(h.deps))
	writes := func() int {
		n := 0
		for _, a := range fakeOf(h).Actions() {
			if a.GetResource().Resource == "configmaps" && (a.GetVerb() == "patch" || a.GetVerb() == "create") {
				n++
			}
		}
		return n
	}
	before := writes()
	if _, isNew, err := h.deps.Approvals.propose(h.deps, *hpaFinding(h.deps).Proposal, "", testNow); err != nil || isNew {
		t.Fatalf("the same change again joins the stored item: new=%v %v", isNew, err)
	}
	if got := writes(); got != before {
		t.Fatalf("a proposal that changes nothing must not write: %d writes", got-before)
	}
}

package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// PLAN-004 phase 4 (B3): the agent's own records. Each must keep what it
// was given across a restart, and a failure to write it must be visible
// (ISS-097).

func readAudit(t *testing.T, path string) []AuditEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuditLog_AppendsAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "audit.jsonl")
	a := NewAuditLog(path)
	a.RecordAction("delete_pod", "default", "deployment/api", "api-1", "CrashLoopBackOff", "success", "deleted pod", "fix")
	a.Close()
	a.Close() // twice is safe

	b := NewAuditLog(path) // a restart appends, never truncates
	b.Record(AuditEntry{Action: "scale_up", Namespace: "default", Workload: "deployment/api", Result: "blocked", Mode: "fix"})
	b.Close()
	got := readAudit(t, path)
	if len(got) != 2 || got[0].Action != "delete_pod" || got[0].Pod != "api-1" || got[1].Result != "blocked" {
		t.Fatalf("audit after a restart: %+v", got)
	}
	if got[0].Timestamp.IsZero() {
		t.Fatal("an entry without a time gets the current time")
	}

	a.Record(AuditEntry{Action: "after_close"}) // a closed log reopens on the next write
	a.Close()
	if n := len(readAudit(t, path)); n != 3 {
		t.Fatalf("a write after Close must reopen and append, got %d entries", n)
	}
}

func TestAuditLog_FailuresAreCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
	open := obs.HandlerErrorsTotal.WithLabelValues("audit", "open")
	before := testutil.ToFloat64(open)
	a := NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	a.Record(AuditEntry{Action: "delete_pod"})
	if got := testutil.ToFloat64(open); got != before+2 {
		t.Fatalf("a failed open at start and at the first write are both counted: %v -> %v", before, got)
	}
	nested := NewAuditLog(filepath.Join(dir, "sub", "audit.jsonl")) // its directory cannot be made
	nested.Close()
	if testutil.ToFloat64(open) != before+3 {
		t.Fatal("a directory that cannot be created is counted")
	}
}

func TestLearningMode_SavesWhereItIsToldAndLoadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "yet", "baselines.json") // a directory that does not exist
	lm := NewLearningMode(path, time.Hour)
	for _, cpu := range []float64{0.2, 0.4, 0.6} {
		lm.RecordCPU("default", "deployment/api", cpu)
	}
	lm.Save()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("baselines must be saved under the configured path, not a fixed directory: %v", err)
	}
	again := NewLearningMode(path, time.Hour)
	bl := again.GetBaseline("default", "deployment/api")
	if bl == nil || bl.SampleCount != 3 || bl.AvgCPU < 0.39 || bl.AvgCPU > 0.41 {
		t.Fatalf("baseline after a restart: %+v", bl)
	}
}

func TestLearningMode_BadFileIsCountedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baselines.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	load := obs.HandlerErrorsTotal.WithLabelValues("learning", "load")
	before := testutil.ToFloat64(load)
	lm := NewLearningMode(path, time.Hour)
	if testutil.ToFloat64(load) != before+1 {
		t.Fatal("a baseline file that does not parse must be counted")
	}
	if len(lm.AllBaselines()) != 0 {
		t.Fatal("a bad file starts with no baselines")
	}
}

func TestLearningMode_ReadersGetCopies(t *testing.T) {
	lm := NewLearningMode(filepath.Join(t.TempDir(), "b.json"), time.Hour)
	lm.RecordCPU("default", "deployment/api", 0.5)
	all := lm.AllBaselines()
	all["default/deployment/api"].AvgCPU = 99
	one := lm.GetBaseline("default", "deployment/api")
	one.SampleCount = 99
	if again := lm.GetBaseline("default", "deployment/api"); again.AvgCPU == 99 || again.SampleCount == 99 {
		t.Fatal("a caller must not be able to change the stored baseline (ISS-096)")
	}
	if lm.GetBaseline("default", "missing") != nil {
		t.Fatal("an unknown workload has no baseline")
	}
}

func TestDeployTracker_DefaultsAndBound(t *testing.T) {
	if dt := NewDeployTracker(0); dt.maxLen != 100 {
		t.Fatalf("default length %d, want 100", dt.maxLen)
	}
	dt := NewDeployTracker(2)
	for i, name := range []string{"a", "b", "c"} {
		dt.RecordDeployAt("default", name, "img", int64(i+1), 1, testNow)
	}
	all := dt.All()
	if len(all) != 2 || all[0].Deployment != "b" || all[1].Deployment != "c" {
		t.Fatalf("the oldest record is dropped when full: %+v", all)
	}
}

func TestFixTracker_ListsAreBounded(t *testing.T) {
	ft := NewFixTracker(1)
	ft.addFixed(FixRecord{Workload: "a"})
	ft.addFixed(FixRecord{Workload: "b"})
	ft.addFailed(FixRecord{Workload: "c"})
	ft.addFailed(FixRecord{Workload: "d"})
	if f := ft.Fixed(); len(f) != 1 || f[0].Workload != "b" {
		t.Fatalf("fixed keeps the newest: %+v", f)
	}
	if f := ft.Failed(); len(f) != 1 || f[0].Workload != "d" {
		t.Fatalf("failed keeps the newest: %+v", f)
	}
	if d := NewFixTracker(0); d.maxLen != 200 {
		t.Fatalf("default length %d, want 200", d.maxLen)
	}
}

func readyPod(name string, ready bool) *corev1.Pod {
	yes := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "worker-5c8d", Controller: &yes}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: ready}}},
	}
}

func TestIsWorkloadHealthy_FallsBackToPodsWithoutADeployment(t *testing.T) {
	ctx := context.Background()
	h := newFindingHarness(t, readyPod("worker-5c8d-a", true), readyPod("worker-5c8d-b", true))
	if ok, detail := isWorkloadHealthy(ctx, h.deps, "default", "replicaset/worker-5c8d"); !ok || detail != "2/2 pods Running+Ready" {
		t.Fatalf("all pods ready: %v %q", ok, detail)
	}

	h = newFindingHarness(t, readyPod("worker-5c8d-a", true), readyPod("worker-5c8d-b", false))
	if ok, detail := isWorkloadHealthy(ctx, h.deps, "default", "replicaset/worker-5c8d"); ok || detail != "1/2 pods ready" {
		t.Fatalf("one pod not ready: %v %q", ok, detail)
	}
	if ok, detail := isWorkloadHealthy(ctx, h.deps, "default", "replicaset/other-1a2b"); ok || detail != "no matching pods" {
		t.Fatalf("no pods: %v %q", ok, detail)
	}
}

func TestIsWorkloadHealthy_ReadErrorsAreCounted(t *testing.T) {
	ctx := context.Background()
	denied := func(res string) k8stesting.ReactionFunc {
		return func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(corev1.Resource(res), "", errors.New("denied"))
		}
	}
	kc := fake.NewSimpleClientset()
	kc.PrependReactor("get", "deployments", denied("deployments"))
	forbidden := obs.APIErrorsTotal.WithLabelValues("deployments", "forbidden")
	before := testutil.ToFloat64(forbidden)
	if ok, detail := isWorkloadHealthy(ctx, &Deps{Client: kc}, "default", "deployment/api"); ok || detail == "" {
		t.Fatalf("a deployment that cannot be read is not healthy: %v %q", ok, detail)
	}
	if testutil.ToFloat64(forbidden) != before+1 {
		t.Fatal("a refused deployment read must be counted")
	}

	kc = fake.NewSimpleClientset()
	kc.PrependReactor("list", "pods", denied("pods"))
	pods := obs.APIErrorsTotal.WithLabelValues("pods", "forbidden")
	before = testutil.ToFloat64(pods)
	if ok, _ := isWorkloadHealthy(ctx, &Deps{Client: kc}, "default", "replicaset/worker-5c8d"); ok {
		t.Fatal("pods that cannot be listed are not healthy")
	}
	if testutil.ToFloat64(pods) != before+1 {
		t.Fatal("a refused pod list must be counted")
	}
}

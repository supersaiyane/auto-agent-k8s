package kube

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
)

// ISS-025: housekeeping (evicted pods, old failed jobs) has its own budget.
// It keeps mode, fix scope, quiet hours and policy limits, but never uses up
// the blast radius, the circuit breaker or the fix rate limit.

func housekeepingChange(applied *int) mutation {
	return mutation{Namespace: "default", Workload: "evicted-pods", Reason: "CleanupEvicted", ActionType: "cleanup_evicted",
		Housekeeping: true, SuccessMsg: "cleaned", SuggestMsg: "clean",
		Apply: func() error { *applied++; return nil }}
}

func TestHousekeeping_HasItsOwnBudget(t *testing.T) {
	h := newFindingHarness(t)
	openGuardrails(h.deps)
	h.deps.BlastRadius = NewBlastRadiusTracker(0, time.Hour)       // no namespace left for fixes
	h.deps.Breaker = ratelimit.NewCircuitBreaker(0, time.Hour)     // every workload tripped
	h.deps.Limiter = ratelimit.NewActionLimiter(0, 10*time.Minute) // no fix budget either
	h.deps.HousekeepingLimiter = ratelimit.NewActionLimiter(2, 10*time.Minute)

	applied := 0
	for i := 0; i < 3; i++ {
		applyMutation(context.Background(), h.deps, housekeepingChange(&applied))
	}
	if applied != 2 {
		t.Fatalf("housekeeping runs on its own budget of 2, ran %d times", applied)
	}

	fix := 0
	out, _ := applyMutation(context.Background(), h.deps, mutation{Namespace: "default", Workload: "deployment/api", Reason: "CrashLoopBackOff",
		ActionType: "delete_pod", SuccessMsg: "deleted", SuggestMsg: "delete", Apply: func() error { fix++; return nil }})
	if out != gateBlocked || fix != 0 {
		t.Fatal("a fix still meets the blast radius, breaker and fix limiter")
	}
}

func TestHousekeeping_KeepsQuietHoursAndFailsClosed(t *testing.T) {
	h := newFindingHarness(t)
	openGuardrails(h.deps)
	h.deps.QuietHours = NewQuietHours("00:00-00:01,00:01-00:00") // always quiet
	applied := 0
	if out, _ := applyMutation(context.Background(), h.deps, housekeepingChange(&applied)); out != gateBlocked || applied != 0 {
		t.Fatal("quiet hours stop housekeeping too")
	}

	h = newFindingHarness(t)
	openGuardrails(h.deps)
	h.deps.HousekeepingLimiter = nil
	if out, _ := applyMutation(context.Background(), h.deps, housekeepingChange(&applied)); out != gateBlocked || applied != 0 {
		t.Fatal("no housekeeping limiter means no budget, not no limit")
	}
}

func TestCleanupEvictedPods_UsesTheHousekeepingBudget(t *testing.T) {
	evicted := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-1", Namespace: "default"},
		Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
	h := newFindingHarness(t, evicted)
	openGuardrails(h.deps)
	h.deps.Limiter = ratelimit.NewActionLimiter(0, 10*time.Minute) // the fix budget is spent
	CleanupEvictedPods(context.Background(), h.deps)
	if _, err := h.deps.Client.CoreV1().Pods("default").Get(context.Background(), "old-1", metav1.GetOptions{}); err == nil {
		t.Fatal("evicted pods are cleaned from the housekeeping budget even when the fix budget is spent")
	}
}

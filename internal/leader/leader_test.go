package leader

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// PLAN-002 9.7: one replica leads at a time, and when the leader stops the
// lease is released and the other replica takes over.
func TestElection_OneLeaderAndHandover(t *testing.T) {
	kc := fake.NewClientset()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	a := Start(ctxA, kc, "auto-agent", "lease", "agent-a")
	waitFor(t, "agent-a to lead", 10*time.Second, a.IsLeader)

	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	b := Start(ctxB, kc, "auto-agent", "lease", "agent-b")
	time.Sleep(300 * time.Millisecond)
	if b.IsLeader() {
		t.Fatal("two replicas must never lead at once")
	}

	cancelA() // ReleaseOnCancel gives the lease up
	waitFor(t, "agent-a to step down", 10*time.Second, func() bool { return !a.IsLeader() })
	waitFor(t, "agent-b to take over", 10*time.Second, b.IsLeader)
}

func TestElection_IdentityFallsBackToHostname(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := Start(ctx, fake.NewClientset(), "auto-agent", "lease", "")
	waitFor(t, "the hostname identity to lead", 10*time.Second, e.IsLeader)
}

// ADR-001: a standby needs to know who leads, to proxy to it.
func TestElection_ReportsTheLeaderIdentity(t *testing.T) {
	kc := fake.NewClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := Start(ctx, kc, "auto-agent", "lease", "agent-a")
	if (&Elector{}).Leader() != "" {
		t.Fatal("no leader known before an election")
	}
	waitFor(t, "the leader identity", 10*time.Second, func() bool { return e.Leader() == "agent-a" })
}

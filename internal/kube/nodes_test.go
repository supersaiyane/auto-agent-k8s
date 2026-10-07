package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
)

// ISS-004: every DaemonSet pod receives every node event. Only the agent on
// the node under pressure may act on it, so N agents produce one cordon.
func TestNodePressure_OnlyAgentOnThatNodeActs(t *testing.T) {
	ctx := context.Background()
	base, kc := newHandlerTestDeps(t)
	n := seedNode(t, ctx, kc, true, false)
	rec := &mutationRecorder{}
	rec.install(kc)

	for _, agentNode := range []string{"node-1", "node-2", "node-3", ""} {
		deps := *base
		deps.Policies = policy.Static(&policy.Policy{Mode: policy.Fix, NamespaceAllow: base.Policy().NamespaceAllow})
		openGuardrails(&deps)
		deps.NodeName = agentNode
		// Each agent is its own pod with its own in-memory dedup, as in a cluster.
		deps.Dedup = ratelimit.NewDeduplicator(time.Minute)
		handleNodePressure(ctx, &deps, n, n)
	}

	cordons := 0
	for _, a := range rec.all() {
		if strings.Contains(a, " nodes/") { // any write to a Node object
			cordons++
		}
	}
	if cordons != 1 {
		t.Fatalf("expected exactly one cordon from the agent on node-1, got %d (%v)", cordons, rec.all())
	}
}

// ISS-004: eviction listed pods in every namespace and ignored the allowlist.
func TestNodePressure_EvictsOnlyAllowlistedNamespaces(t *testing.T) {
	ctx := context.Background()
	deps, kc := newHandlerTestDeps(t)
	openGuardrails(deps)
	deps.NodeName = "node-1"
	n := seedNode(t, ctx, kc, true, false)
	seedPod(t, ctx, kc, "allowed", "node-1") // namespace default, allowlisted
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "not-allowed", Namespace: "payments"},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "app", Image: "x"}}},
	}
	_, err := kc.CoreV1().Pods("payments").Create(ctx, other, metav1.CreateOptions{})
	mustCreate(t, err)
	rec := &mutationRecorder{}
	rec.install(kc)

	handleNodePressure(ctx, deps, n, n)

	var evictions []string
	for _, a := range rec.all() {
		if strings.HasSuffix(a, "/eviction") {
			evictions = append(evictions, a)
		}
	}
	if len(evictions) != 1 {
		t.Fatalf("expected one eviction (namespace default only), got %v", rec.all())
	}
}

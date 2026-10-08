package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func node(name string, conds []corev1.NodeCondition, mut func(*corev1.Node)) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Conditions: conds}}
	if mut != nil {
		mut(n)
	}
	return n
}

func readyCond(st corev1.ConditionStatus, reason, msg string, since, heartbeat time.Duration) corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeReady, Status: st, Reason: reason, Message: msg,
		LastTransitionTime: metav1.NewTime(testNow.Add(-since)), LastHeartbeatTime: metav1.NewTime(testNow.Add(-heartbeat))}
}

// Phase 12 audit of CheckNodeHealth.
//
// Claims: nodes NotReady, from the leader.
// Bugs found: (1) a node NotReady for a second (joining, a blip) paged at
// once; now after nodeNotReadyFor; (2) a failed node list was dropped; (3)
// the pod count read every namespace (constraint 4), now only watched ones;
// (4) a runtime failure was reported twice, as NodeNotReady and as
// ContainerRuntimeDown; now only as the latter. Rung R0: the cause is on
// the node.
func TestAudit_NodeHealth(t *testing.T) {
	h := newFindingHarness(t,
		node("down", []corev1.NodeCondition{readyCond(corev1.ConditionFalse, "KubeletNotReady", "PLEG is not healthy", 10*time.Minute, time.Minute)}, nil),
		node("blip", []corev1.NodeCondition{readyCond(corev1.ConditionFalse, "KubeletNotReady", "x", 30*time.Second, 0)}, nil),
		node("runtime", []corev1.NodeCondition{readyCond(corev1.ConditionFalse, "KubeletNotReady", "container runtime is down", 10*time.Minute, 0)}, nil),
		node("fine", []corev1.NodeCondition{readyCond(corev1.ConditionTrue, "KubeletReady", "", time.Hour, 0)}, nil),
		pod("on-down", func(p *corev1.Pod) { p.Spec.NodeName = "down" }),
		pod("hidden", func(p *corev1.Pod) { p.Namespace = "payments"; p.Spec.NodeName = "down" }))
	CheckNodeHealth(context.Background(), h.deps)
	m := h.expect(t, "NodeNotReady", RungAlert, 1)[0]
	for _, want := range []string{"node/down", "NotReady for 10m0s (KubeletNotReady)", "PLEG is not healthy", "watched pods on the node: 1"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
	expectCounted(t, "nodes", func(d *Deps) { CheckNodeHealth(context.Background(), d) })
}

// Phase 12 audit of CheckNodeExtended.
//
// Claims: PID pressure, network unavailable, container runtime down, a
// forgotten cordon, clock skew.
// Bugs found: (1) ContainerRuntimeDown fired for any KubeletNotReady, the
// kubelet's generic reason; now only when the message names the runtime;
// (2) "cordoned for more than an hour" was measured from the node's creation,
// so cordoning an old node for maintenance alerted at once; now from the
// unschedulable taint's TimeAdded, then once a day; (3) clock skew flagged
// any Ready node whose heartbeat was over five minutes old, which is the
// kubelet's normal refresh; now only a heartbeat from the future or one much
// older than the refresh.
func TestAudit_NodeExtended(t *testing.T) {
	cordon := func(n *corev1.Node, d time.Duration) {
		at := metav1.NewTime(testNow.Add(-d))
		n.Spec.Unschedulable = true
		n.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule, TimeAdded: &at}}
		n.CreationTimestamp = metav1.NewTime(testNow.Add(-90 * 24 * time.Hour))
	}
	ready := readyCond(corev1.ConditionTrue, "KubeletReady", "", time.Hour, time.Minute)
	h := newFindingHarness(t,
		node("pids", []corev1.NodeCondition{ready, {Type: corev1.NodePIDPressure, Status: corev1.ConditionTrue}}, nil),
		node("cni", []corev1.NodeCondition{ready, {Type: corev1.NodeNetworkUnavailable, Status: corev1.ConditionTrue, Message: "no CNI config"}}, nil),
		node("rt", []corev1.NodeCondition{readyCond(corev1.ConditionFalse, "KubeletNotReady", "container runtime network not ready", 10*time.Minute, 0)}, nil),
		node("generic", []corev1.NodeCondition{readyCond(corev1.ConditionFalse, "KubeletNotReady", "PLEG is not healthy", 10*time.Minute, 0)}, nil),
		node("forgot", []corev1.NodeCondition{ready}, func(n *corev1.Node) { cordon(n, 3*time.Hour) }),
		node("maint", []corev1.NodeCondition{ready}, func(n *corev1.Node) { cordon(n, 5*time.Minute) }),
		node("ours", []corev1.NodeCondition{ready}, func(n *corev1.Node) {
			cordon(n, 3*time.Hour)
			n.Annotations = map[string]string{"auto-agent.io/cordoned": "true"}
		}),
		node("ahead", []corev1.NodeCondition{readyCond(corev1.ConditionTrue, "KubeletReady", "", time.Hour, -10*time.Minute)}, nil),
		node("behind", []corev1.NodeCondition{readyCond(corev1.ConditionTrue, "KubeletReady", "", time.Hour, 40*time.Minute)}, nil),
		node("normal", []corev1.NodeCondition{readyCond(corev1.ConditionTrue, "KubeletReady", "", time.Hour, 6*time.Minute)}, nil))
	CheckNodeExtended(context.Background(), h.deps)
	CheckNodeExtended(context.Background(), h.deps)
	h.expect(t, "PIDPressure", RungAlert, 1)
	if m := h.expect(t, "NetworkUnavailable", RungAlert, 1)[0]; !strings.Contains(m, "no CNI config") {
		t.Fatalf("network: %s", m)
	}
	if m := h.expect(t, "ContainerRuntimeDown", RungAlert, 1)[0]; !strings.Contains(m, "node/rt") {
		t.Fatalf("runtime: %s", m)
	}
	if m := h.expect(t, "CordonedForgotten", RungGuided, 1)[0]; !strings.Contains(m, "node/forgot") || !strings.Contains(m, "cordoned for 3h0m0s") {
		t.Fatalf("cordon: %s", m)
	}
	skew := strings.Join(h.expect(t, "ClockSkew", RungAlert, 2), "\n")
	if !strings.Contains(skew, "node/ahead") || !strings.Contains(skew, "in the future") || !strings.Contains(skew, "node/behind") || strings.Contains(skew, "node/normal") {
		t.Fatalf("skew: %s", skew)
	}
	expectCounted(t, "nodes", func(d *Deps) { CheckNodeExtended(context.Background(), d) })
}

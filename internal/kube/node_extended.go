package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// CheckNodeExtended reports node conditions beyond memory and disk
// pressure: PID pressure, network unavailable, the container runtime down, a
// cordon left in place, and clock skew. Runs on the leader. These causes are
// on the node, so the rung is R0 except the forgotten cordon (R1).
func CheckNodeExtended(ctx context.Context, deps *Deps) {
	nodes, err := deps.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "nodes", "")
		return
	}
	now := deps.clock()
	for i := range nodes.Items {
		for _, f := range nodeFindings(&nodes.Items[i], now) {
			if f.Reason == "CordonedForgotten" && !deps.Dedup.CheckFor(dedupKey("", f.Node, "CordonedDaily"), 24*time.Hour) {
				continue
			}
			report(ctx, deps, f)
		}
	}
}

// nodeFindings lists what is wrong with one node.
func nodeFindings(node *corev1.Node, now time.Time) []finding {
	base := finding{Workload: "node/" + node.Name, Node: node.Name, Severity: eventsvc.SevCritical, Rung: RungAlert}
	var out []finding
	if c := nodeCondition(node, corev1.NodePIDPressure); c != nil && c.Status == corev1.ConditionTrue {
		f := base
		f.Reason, f.Summary = "PIDPressure", "too many processes; the kubelet may evict pods"
		f.Fix = "find the pod forking processes (a runaway sidecar or a fork bomb); set pod PID limits"
		out = append(out, f)
	}
	if c := nodeCondition(node, corev1.NodeNetworkUnavailable); c != nil && c.Status == corev1.ConditionTrue {
		f := base
		f.Reason, f.Summary, f.Details = "NetworkUnavailable", "pod networking is not configured on this node", []string{c.Message}
		f.Fix = "check the CNI plugin's pod on this node (Calico, Cilium, Flannel) and the node's network setup"
		out = append(out, f)
	}
	if c := nodeCondition(node, corev1.NodeReady); c != nil && runtimeDown(c) && now.Sub(c.LastTransitionTime.Time) >= nodeNotReadyFor {
		f := base
		f.Reason, f.Summary, f.Details = "ContainerRuntimeDown", "the container runtime is not ready", []string{c.Reason + ": " + c.Message}
		f.Fix = "restart and inspect containerd (or the configured runtime) and the kubelet on this node"
		out = append(out, f)
	}
	if f, ok := forgottenCordon(node, now, base); ok {
		out = append(out, f)
	}
	if f, ok := clockSkew(node, now, base); ok {
		out = append(out, f)
	}
	return out
}

// cordonedFor is how long a Ready node may stay cordoned before it is
// reported as forgotten.
const cordonedFor = time.Hour

// forgottenCordon reports a Ready node cordoned for cordonedFor by someone
// other than the agent. The cordon time is the unschedulable taint's
// TimeAdded; the audit found it measured from the node's creation instead.
func forgottenCordon(node *corev1.Node, now time.Time, base finding) (finding, bool) {
	if !node.Spec.Unschedulable || node.Annotations["auto-agent.io/cordoned"] == "true" {
		return finding{}, false
	}
	if c := nodeCondition(node, corev1.NodeReady); c == nil || c.Status != corev1.ConditionTrue {
		return finding{}, false
	}
	var since *metav1.Time
	for _, t := range node.Spec.Taints {
		if t.Key == corev1.TaintNodeUnschedulable {
			since = t.TimeAdded
		}
	}
	if since == nil || now.Sub(since.Time) < cordonedFor {
		return finding{}, false
	}
	f := base
	f.Reason, f.Severity, f.Rung = "CordonedForgotten", eventsvc.SevInfo, RungGuided
	f.Summary = fmt.Sprintf("Ready but cordoned for %s; nothing new is scheduled here", now.Sub(since.Time).Round(time.Minute))
	f.Fix = fmt.Sprintf("if the maintenance is done: `kubectl uncordon %s`", node.Name)
	return f, true
}

// Clock skew thresholds. The kubelet refreshes the Ready heartbeat only
// every few minutes when nothing changes (nodeStatusReportFrequency, 5m by
// default), so an old heartbeat alone is normal; the audit found a 5 minute
// threshold that flagged healthy nodes. A heartbeat from the future, or one
// much older than that refresh while the node is still Ready, is not.
const (
	skewAhead  = 2 * time.Minute
	skewBehind = 15 * time.Minute
)

func clockSkew(node *corev1.Node, now time.Time, base finding) (finding, bool) {
	c := nodeCondition(node, corev1.NodeReady)
	if c == nil || c.Status != corev1.ConditionTrue || c.LastHeartbeatTime.IsZero() {
		return finding{}, false
	}
	gap := now.Sub(c.LastHeartbeatTime.Time)
	if gap > -skewAhead && gap < skewBehind {
		return finding{}, false
	}
	f := base
	f.Reason, f.Severity = "ClockSkew", eventsvc.SevWarning
	if gap < 0 {
		f.Summary = fmt.Sprintf("the node's heartbeat is %s in the future: its clock runs ahead", (-gap).Round(time.Second))
	} else {
		f.Summary = fmt.Sprintf("the node reports Ready with a heartbeat %s old: its clock may run behind", gap.Round(time.Second))
	}
	f.Fix = "check NTP or chrony on the node; certificates and leases fail when clocks drift"
	return f, true
}

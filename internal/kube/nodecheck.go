package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// nodeNotReadyFor is how long a node must stay NotReady before it is
// reported, so a node joining or a short blip does not page (phase 12 audit).
const nodeNotReadyFor = 2 * time.Minute

// CheckNodeHealth reports nodes NotReady for nodeNotReadyFor. Runs on the
// leader, so it works even when the node's own agent is down. A node whose
// message names the container runtime is reported by CheckNodeExtended as
// ContainerRuntimeDown instead, not twice.
func CheckNodeHealth(ctx context.Context, deps *Deps) {
	nodes, err := deps.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "nodes", "") // phase 12: was a silent return
		return
	}
	now := deps.clock()
	for i := range nodes.Items {
		node := &nodes.Items[i]
		c := nodeCondition(node, corev1.NodeReady)
		if c == nil || c.Status == corev1.ConditionTrue || runtimeDown(c) || now.Sub(c.LastTransitionTime.Time) < nodeNotReadyFor {
			continue
		}
		report(ctx, deps, finding{Reason: "NodeNotReady", Workload: "node/" + node.Name, Node: node.Name,
			Severity: eventsvc.SevCritical, Rung: RungAlert,
			Summary: fmt.Sprintf("NotReady for %s (%s)", now.Sub(c.LastTransitionTime.Time).Round(time.Minute), c.Reason),
			Details: []string{c.Message, fmt.Sprintf("Kubelet %s, %s; watched pods on the node: %d",
				node.Status.NodeInfo.KubeletVersion, node.Status.NodeInfo.OSImage, podsOnNode(ctx, deps, node.Name))},
			Fix: "the cause is on the node: check the kubelet and container runtime logs, disk, memory and network there"})
	}
}

// nodeCondition returns the node's condition of type t, or nil.
func nodeCondition(n *corev1.Node, t corev1.NodeConditionType) *corev1.NodeCondition {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == t {
			return &n.Status.Conditions[i]
		}
	}
	return nil
}

// runtimeDown reports whether a NotReady condition names the container
// runtime; KubeletNotReady alone is the kubelet's generic reason.
func runtimeDown(c *corev1.NodeCondition) bool {
	return c.Status != corev1.ConditionTrue && strings.Contains(strings.ToLower(c.Message), "container runtime")
}

// podsOnNode counts pods on the node in watched namespaces only: reads
// follow the watch scope (constraint 4; the audit found a cluster-wide read).
func podsOnNode(ctx context.Context, deps *Deps, node string) int {
	n := 0
	for _, ns := range watchedNamespaces(ctx, deps) {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
		if err != nil {
			countAPIError(err, "pods", ns)
			continue
		}
		for i := range pods.Items {
			if pods.Items[i].Spec.NodeName == node {
				n++
			}
		}
	}
	return n
}

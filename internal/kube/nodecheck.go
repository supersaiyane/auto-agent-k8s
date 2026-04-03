package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckNodeHealth detects nodes in NotReady state. Runs from the leader
// so it works even if the affected node's agent pod is down.
func CheckNodeHealth(ctx context.Context, deps *Deps) {
	nodes, err := deps.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(3).Infof("nodecheck: failed to list nodes: %v", err)
		return
	}

	for _, node := range nodes.Items {
		ready := false
		var readyCond *corev1.NodeCondition
		for i, c := range node.Status.Conditions {
			if c.Type == corev1.NodeReady {
				readyCond = &node.Status.Conditions[i]
				if c.Status == corev1.ConditionTrue {
					ready = true
				}
				break
			}
		}

		if ready {
			continue
		}

		key := dedupKey("", node.Name, "NodeNotReady")
		if !deps.Dedup.Check(key) {
			obs.DedupSkippedTotal.WithLabelValues("NodeNotReady").Inc()
			continue
		}

		klog.Infof("nodecheck: node %s is NotReady", node.Name)

		msg := fmt.Sprintf("*NodeNotReady*: `%s` is not ready\n", node.Name)
		msg += fmt.Sprintf("Version: %s, OS: %s\n", node.Status.NodeInfo.KubeletVersion, node.Status.NodeInfo.OSImage)

		if readyCond != nil {
			msg += fmt.Sprintf("Reason: %s\nMessage: %s\n", readyCond.Reason, readyCond.Message)
			msg += fmt.Sprintf("Last transition: %s\n", readyCond.LastTransitionTime.Format("2006-01-02 15:04:05"))
		}

		// Count pods on this node
		pods, _ := deps.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + node.Name,
		})
		podCount := 0
		if pods != nil {
			podCount = len(pods.Items)
		}
		msg += fmt.Sprintf("Pods on node: %d\n", podCount)
		msg += "_Action required_: investigate kubelet, container runtime, and network on this node.\n"

		deps.Slack.Post(msg)
		fireAlert(ctx, deps, "NodeNotReady", "", node.Name, "", msg, "critical")
		recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
			Node: node.Name, Reason: "NodeNotReady",
			Message: fmt.Sprintf("Node not ready (%d pods affected)", podCount)})
		obs.IncidentsTotal.WithLabelValues("NodeNotReady", "", node.Name).Inc()
	}
}

package kube

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckNodeExtended detects additional node conditions beyond memory/disk pressure.
// Runs from leader.
func CheckNodeExtended(ctx context.Context, deps *Deps) {
	nodes, err := deps.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, node := range nodes.Items {
		checkPIDPressure(ctx, deps, &node)
		checkNetworkUnavailable(ctx, deps, &node)
		checkContainerRuntime(ctx, deps, &node)
		checkCordonedForgotten(ctx, deps, &node)
		checkClockSkew(ctx, deps, &node)
	}
}

func checkPIDPressure(ctx context.Context, deps *Deps, node *corev1.Node) {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodePIDPressure && c.Status == corev1.ConditionTrue {
			key := dedupKey("", node.Name, "PIDPressure")
			if !deps.Dedup.Check(key) {
				return
			}
			msg := fmt.Sprintf("*PIDPressure* on node `%s`\nToo many processes running — pods may be evicted.\n", node.Name)
			msg += "_Action_: investigate runaway processes, check for fork bombs or misconfigured sidecars.\n"
			deps.Slack.Post(msg)
			fireAlert(ctx, deps, "PIDPressure", "", node.Name, "", msg, "critical")
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
				Node: node.Name, Reason: "PIDPressure", Message: "Too many processes on node"})
			obs.IncidentsTotal.WithLabelValues("PIDPressure", "", node.Name).Inc()
			return
		}
	}
}

func checkNetworkUnavailable(ctx context.Context, deps *Deps, node *corev1.Node) {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeNetworkUnavailable && c.Status == corev1.ConditionTrue {
			key := dedupKey("", node.Name, "NetworkUnavailable")
			if !deps.Dedup.Check(key) {
				return
			}
			msg := fmt.Sprintf("*NetworkUnavailable* on node `%s`\n", node.Name)
			msg += fmt.Sprintf("Reason: %s\n", c.Message)
			msg += "_Check_: CNI plugin (Calico/Cilium/Flannel), node network config.\n"
			deps.Slack.Post(msg)
			fireAlert(ctx, deps, "NetworkUnavailable", "", node.Name, "", msg, "critical")
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
				Node: node.Name, Reason: "NetworkUnavailable", Message: c.Message})
			obs.IncidentsTotal.WithLabelValues("NetworkUnavailable", "", node.Name).Inc()
			return
		}
	}
}

func checkContainerRuntime(ctx context.Context, deps *Deps, node *corev1.Node) {
	// Detect container runtime issues via Ready condition reason
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			if c.Reason == "KubeletNotReady" || c.Reason == "ContainerRuntimeNotReady" {
				key := dedupKey("", node.Name, "RuntimeDown-"+c.Reason)
				if !deps.Dedup.Check(key) {
					return
				}
				msg := fmt.Sprintf("*ContainerRuntimeDown* on node `%s`\n", node.Name)
				msg += fmt.Sprintf("Reason: %s — %s\n", c.Reason, c.Message)
				msg += "_Action_: check kubelet and container runtime (containerd/docker) on this node.\n"
				deps.Slack.Post(msg)
				fireAlert(ctx, deps, "ContainerRuntimeDown", "", node.Name, "", msg, "critical")
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
					Node: node.Name, Reason: "ContainerRuntimeDown", Message: c.Reason + ": " + c.Message})
				obs.IncidentsTotal.WithLabelValues("ContainerRuntimeDown", "", node.Name).Inc()
				return
			}
		}
	}
}

func checkCordonedForgotten(ctx context.Context, deps *Deps, node *corev1.Node) {
	if !node.Spec.Unschedulable {
		return
	}
	// Skip if we cordoned it ourselves
	if node.Annotations != nil && node.Annotations["auto-agent.io/cordoned"] == "true" {
		return
	}
	// Only alert if cordoned for more than 1 hour
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			// Node is Ready but cordoned — someone forgot
			if time.Since(node.CreationTimestamp.Time) < 1*time.Hour {
				return // recently created, might be intentional
			}
			key := dedupKey("", node.Name, "CordonedForgotten")
			if !deps.Dedup.Check(key) {
				return
			}
			msg := fmt.Sprintf("*CordonedForgotten* node `%s` is Ready but still cordoned (unschedulable)\n", node.Name)
			msg += "_Check_: was this intentional? If maintenance is done, uncordon: `kubectl uncordon %s`\n"
			msg = fmt.Sprintf(msg, node.Name)
			deps.Slack.Post(msg)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevInfo,
				Node: node.Name, Reason: "CordonedForgotten", Message: "Node Ready but still cordoned"})
			obs.IncidentsTotal.WithLabelValues("CordonedForgotten", "", node.Name).Inc()
			return
		}
	}
}

func checkClockSkew(ctx context.Context, deps *Deps, node *corev1.Node) {
	// Check if node's last heartbeat is significantly off
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			heartbeat := c.LastHeartbeatTime.Time
			if heartbeat.IsZero() {
				return
			}
			skew := math.Abs(time.Since(heartbeat).Seconds())
			// If last heartbeat is >5 minutes old but node claims Ready, clock may be skewed
			if skew > 300 && c.Status == corev1.ConditionTrue {
				key := dedupKey("", node.Name, "ClockSkew")
				if !deps.Dedup.Check(key) {
					return
				}
				msg := fmt.Sprintf("*ClockSkew* possible on node `%s`\n", node.Name)
				msg += fmt.Sprintf("Last heartbeat: %s (%.0fs ago)\n", heartbeat.Format(time.RFC3339), skew)
				msg += "_Check_: NTP/chrony sync on this node. TLS certs may fail with clock drift.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Node: node.Name, Reason: "ClockSkew",
					Message: fmt.Sprintf("Last heartbeat %.0fs ago", skew)})
				obs.IncidentsTotal.WithLabelValues("ClockSkew", "", node.Name).Inc()
				return
			}
		}
	}
}

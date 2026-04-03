package kube

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
)

// CheckStuckRollouts scans deployments for ProgressDeadlineExceeded and optionally rolls back.
// Must be called only by the leader.
func CheckStuckRollouts(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		deployments, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.V(3).Infof("rollouts: failed to list deployments in %s: %v", ns, err)
			continue
		}

		for i := range deployments.Items {
			d := &deployments.Items[i]
			if isRolloutStuck(d) {
				key := dedupKey(ns, d.Name, "RolloutStuck")
				if !deps.Dedup.Check(key) {
					obs.DedupSkippedTotal.WithLabelValues("RolloutStuck").Inc()
					continue
				}
				handleStuckRollout(ctx, deps, d)
			}
		}
	}
}

func isRolloutStuck(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing &&
			c.Status == corev1.ConditionFalse &&
			c.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func handleStuckRollout(ctx context.Context, deps *Deps, d *appsv1.Deployment) {
	ns := d.Namespace
	name := d.Name
	klog.Infof("handler: stuck rollout detected %s/%s (ProgressDeadlineExceeded)", ns, name)

	events := collectEvents(ctx, deps.Client, ns, name)
	url, _ := persistLogBundle(ctx, deps.Sink, ns, name, name, "", "",
		"RolloutStuck", "ProgressDeadlineExceeded", "", events)

	// Get revision info
	var currentRev string
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			currentRev = c.Message
			break
		}
	}

	msg := fmt.Sprintf("*RolloutStuck* on `%s/%s` — ProgressDeadlineExceeded\n", ns, name)
	msg += fmt.Sprintf("Replicas: %d desired, %d updated, %d available\n",
		valueOr(d.Spec.Replicas, 1), d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
	if currentRev != "" {
		msg += fmt.Sprintf("Progress: %s\n", currentRev)
	}
	msg += fmt.Sprintf("Saved: `%s`\n", url)

	if deps.Policy.Mode == policy.Fix {
		crdPol := effectivePolicy(deps, ns, d.Spec.Template.Labels)
		if policyAllowsAction(crdPol) && checkBreaker(ctx, deps, ns, name) {
			// Rollback: undo the last rollout by updating the deployment's revision
			// The safest approach is to use the rollout undo equivalent
			rollbackMsg := rollbackDeployment(ctx, deps, ns, name)
			msg += rollbackMsg
		}
	} else {
		msg += "_Suggest_: run `kubectl rollout undo deployment/" + name + " -n " + ns + "` to rollback.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Deployment rollout stuck", strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "RolloutStuck", ns, name, "", msg, "critical")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: name, Reason: "RolloutStuck",
		Message: fmt.Sprintf("ProgressDeadlineExceeded (%d/%d available)", d.Status.AvailableReplicas, valueOr(d.Spec.Replicas, 1)),
		LogURL: url})
	createTicket(ctx, deps, fmt.Sprintf("rollout-%s-%s", ns, name),
		fmt.Sprintf("RolloutStuck: %s/%s", ns, name), msg)
	obs.IncidentsTotal.WithLabelValues("RolloutStuck", ns, name).Inc()
}

// rollbackDeployment triggers a rollback by patching the deployment's revision annotation.
func rollbackDeployment(ctx context.Context, deps *Deps, ns, name string) string {
	// Get deployment's revision history
	rsList, err := deps.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("_Action_: rollback failed (cannot list ReplicaSets): %v\n", err)
	}

	// Find the previous (second-highest) revision ReplicaSet for this deployment
	var prevRS *appsv1.ReplicaSet
	var maxRev, prevRev int64
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		// Check if owned by this deployment
		owned := false
		for _, ref := range rs.OwnerReferences {
			if ref.Kind == "Deployment" && ref.Name == name {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		rev := parseRevision(rs.Annotations["deployment.kubernetes.io/revision"])
		if rev > maxRev {
			prevRev = maxRev
			prevRS = nil
			maxRev = rev
		}
		if rev == prevRev && prevRev > 0 {
			prevRS = rs
		}
		if rev > prevRev && rev < maxRev {
			prevRev = rev
			prevRS = rs
		}
	}

	if prevRS == nil {
		return "_Action_: rollback skipped — no previous revision found.\n"
	}

	// Patch deployment spec with previous RS template
	deploy, err := deps.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("_Action_: rollback failed: %v\n", err)
	}

	deploy.Spec.Template = prevRS.Spec.Template
	if deploy.Annotations == nil {
		deploy.Annotations = map[string]string{}
	}
	deploy.Annotations["auto-agent.io/rollback-from"] = fmt.Sprintf("%d", maxRev)
	deploy.Annotations["auto-agent.io/rollback-to"] = fmt.Sprintf("%d", prevRev)

	if _, err := deps.Client.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Sprintf("_Action_: rollback failed: %v\n", err)
	}

	obs.ActionsTotal.WithLabelValues("rollback", ns, name).Inc()
	return fmt.Sprintf("_Action_: rolled back from revision %d to %d.\n", maxRev, prevRev)
}

// CleanupEvictedPods removes pods in the Evicted/Failed state that are consuming etcd space.
// Must be called only by the leader.
func CleanupEvictedPods(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Failed",
		})
		if err != nil {
			klog.V(3).Infof("evicted: failed to list in %s: %v", ns, err)
			continue
		}

		cleaned := 0
		for _, pod := range pods.Items {
			if pod.Status.Reason == "Evicted" || pod.Status.Reason == "NodeAffinity" ||
				pod.Status.Reason == "Shutdown" || pod.Status.Reason == "UnexpectedAdmissionError" {
				if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err == nil {
					cleaned++
				}
			}
		}
		if cleaned > 0 {
			klog.Infof("evicted: cleaned %d failed/evicted pods in %s", cleaned, ns)
			obs.ActionsTotal.WithLabelValues("cleanup_evicted", ns, "").Inc()
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Action, Severity: eventsvc.SevInfo,
				Namespace: ns, Reason: "CleanupEvicted",
				Message: fmt.Sprintf("Removed %d evicted/failed pods", cleaned)})
		}
	}
}

// CheckServiceEndpoints detects services with 0 ready endpoints.
// Must be called only by the leader.
func CheckServiceEndpoints(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		endpoints, err := deps.Client.CoreV1().Endpoints(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ep := range endpoints.Items {
			readyCount := 0
			for _, subset := range ep.Subsets {
				readyCount += len(subset.Addresses)
			}
			if readyCount == 0 && len(ep.Subsets) > 0 {
				key := dedupKey(ns, ep.Name, "NoEndpoints")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*NoEndpoints* service `%s/%s` has 0 ready endpoints — traffic blackhole\n", ns, ep.Name)
				msg += "_Check_: pods matching service selector, readiness probes, and pod scheduling.\n"
				deps.Slack.Post(msg)
				fireAlert(ctx, deps, "NoEndpoints", ns, ep.Name, "", msg, "critical")
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
					Namespace: ns, Workload: ep.Name, Reason: "NoEndpoints",
					Message: "Service has 0 ready endpoints"})
				obs.IncidentsTotal.WithLabelValues("NoEndpoints", ns, ep.Name).Inc()
			}
		}
	}
}

func valueOr(p *int32, def int32) int32 {
	if p != nil {
		return *p
	}
	return def
}

func parseRevision(s string) int64 {
	var rev int64
	fmt.Sscanf(s, "%d", &rev)
	return rev
}


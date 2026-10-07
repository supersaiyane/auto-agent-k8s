package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// CheckStuckRollouts scans deployments for ProgressDeadlineExceeded and optionally rolls back.
// Must be called only by the leader.
func CheckStuckRollouts(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy().NamespaceAllow {
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
	url, err := persistLogBundle(ctx, deps.Sink, ns, name, name, "", "",
		"RolloutStuck", "ProgressDeadlineExceeded", "", events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("logbundle", "storage").Inc()
	}

	// Get revision info
	var currentRev string
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing {
			currentRev = c.Message
			break
		}
	}

	msg := fmt.Sprintf("*RolloutStuck* on `%s/%s`: ProgressDeadlineExceeded\n", ns, name)
	msg += fmt.Sprintf("Replicas: %d desired, %d updated, %d available\n",
		valueOr(d.Spec.Replicas, 1), d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
	if currentRev != "" {
		msg += fmt.Sprintf("Progress: %s\n", currentRev)
	}
	msg += fmt.Sprintf("Saved: `%s`\n", url)

	msg += rollbackDeployment(ctx, deps, ns, name, d.Spec.Template.Labels)

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Deployment rollout stuck", strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "RolloutStuck", ns, name, "", msg, "critical")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: name, Reason: "RolloutStuck",
		Message: fmt.Sprintf("ProgressDeadlineExceeded (%d/%d available)", d.Status.AvailableReplicas, valueOr(d.Spec.Replicas, 1)),
		LogURL:  url})
	createTicket(ctx, deps, fmt.Sprintf("rollout-%s-%s", ns, name),
		fmt.Sprintf("RolloutStuck: %s/%s", ns, name), msg)
	obs.IncidentsTotal.WithLabelValues("RolloutStuck", ns, name).Inc()
}

// rollbackDeployment rolls a deployment back to its previous ReplicaSet
// template. Finding the target revision only reads; the write goes through
// the mutation gate.
func rollbackDeployment(ctx context.Context, deps *Deps, ns, name string, labels map[string]string) string {
	rsList, err := deps.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("_Action_: rollback failed (cannot list ReplicaSets): %v\n", err)
	}

	// Find the newest and the second newest revision owned by this deployment.
	// The result must not depend on list order (ISS-023).
	var curRS, prevRS *appsv1.ReplicaSet
	var maxRev, prevRev int64
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !ownedByDeployment(rs, name) {
			continue
		}
		rev := parseRevision(rs.Annotations["deployment.kubernetes.io/revision"])
		switch {
		case rev > maxRev:
			prevRS, prevRev = curRS, maxRev
			curRS, maxRev = rs, rev
		case rev > prevRev && rev < maxRev:
			prevRS, prevRev = rs, rev
		}
	}
	if prevRS == nil {
		return "_Action_: rollback skipped, no previous revision found.\n"
	}

	// The template is replaced whole, as `kubectl rollout undo` does, minus
	// the ReplicaSet's own pod-template-hash label.
	template := *prevRS.Spec.Template.DeepCopy()
	delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)

	return tryFixAction(ctx, deps, ns, name, "", labels, "RolloutStuck", "rollback",
		func() error {
			// Re-read on every attempt so a concurrent change is never overwritten.
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				deploy, err := deps.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				deploy.Spec.Template = template
				if deploy.Annotations == nil {
					deploy.Annotations = map[string]string{}
				}
				deploy.Annotations["auto-agent.io/rollback-from"] = fmt.Sprintf("%d", maxRev)
				deploy.Annotations["auto-agent.io/rollback-to"] = fmt.Sprintf("%d", prevRev)
				_, err = deps.Client.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{})
				return err
			})
		},
		fmt.Sprintf("rolled back from revision %d to %d", maxRev, prevRev),
		fmt.Sprintf("run `kubectl rollout undo deployment/%s -n %s` to roll back to revision %d", name, ns, prevRev))
}

func ownedByDeployment(rs *appsv1.ReplicaSet, name string) bool {
	for _, ref := range rs.OwnerReferences {
		if ref.Kind == "Deployment" && ref.Name == name {
			return true
		}
	}
	return false
}

// evictedReasons are the pod status reasons CleanupEvictedPods removes.
var evictedReasons = map[string]bool{
	"Evicted": true, "NodeAffinity": true, "Shutdown": true, "UnexpectedAdmissionError": true,
}

// CleanupEvictedPods removes pods in the Evicted/Failed state that are consuming etcd space.
// Each namespace is one gated action, so the guardrails count a cleanup run
// once rather than once per dead pod.
// Must be called only by the leader.
func CleanupEvictedPods(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy().NamespaceAllow {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Failed",
		})
		if err != nil {
			klog.V(3).Infof("evicted: failed to list in %s: %v", ns, err)
			continue
		}

		var names []string
		for _, pod := range pods.Items {
			if evictedReasons[pod.Status.Reason] {
				names = append(names, pod.Name)
			}
		}
		if len(names) == 0 {
			continue
		}

		cleaned := 0
		outcome, gmsg := applyMutation(ctx, deps, mutation{
			Namespace: ns, Workload: "evicted-pods", Reason: "CleanupEvicted", ActionType: "cleanup_evicted",
			SuccessMsg: fmt.Sprintf("removed evicted or failed pods in %s", ns),
			SuggestMsg: fmt.Sprintf("remove %d evicted or failed pods in %s", len(names), ns),
			Apply: func() error {
				var errs []error
				for _, n := range names {
					if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, n, metav1.DeleteOptions{}); err != nil {
						errs = append(errs, fmt.Errorf("delete pod %s/%s: %w", ns, n, err))
						continue
					}
					cleaned++
				}
				return errors.Join(errs...)
			},
		})
		if outcome == gateSuggested {
			klog.V(2).Infof("evicted: %s", strings.TrimSpace(gmsg))
		}
		if cleaned > 0 {
			klog.Infof("evicted: cleaned %d failed/evicted pods in %s", cleaned, ns)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Action, Severity: eventsvc.SevInfo,
				Namespace: ns, Reason: "CleanupEvicted",
				Message: fmt.Sprintf("Removed %d evicted/failed pods", cleaned)})
		}
	}
}

// CheckServiceEndpoints detects services with 0 ready endpoints.
// Must be called only by the leader.
func CheckServiceEndpoints(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy().NamespaceAllow {
		endpoints, err := deps.Client.CoreV1().Endpoints(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "endpoints", ns)
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
				msg := fmt.Sprintf("*NoEndpoints* service `%s/%s` has 0 ready endpoints: traffic blackhole\n", ns, ep.Name)
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

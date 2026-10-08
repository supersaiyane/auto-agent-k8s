package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// CheckStuckRollouts scans deployments for ProgressDeadlineExceeded and optionally rolls back.
// Must be called only by the leader.
func CheckStuckRollouts(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		deployments, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "deployments", ns) // phase 12: was a silent continue
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
	msg += fmt.Sprintf("_Rung %s, %s_\n", RungAuto, rungNames[RungAuto])
	if err := deps.Slack.Post(msg); err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("rollout", "slack").Inc()
	}
	fireAlert(ctx, deps, "RolloutStuck", ns, name, "", msg, "critical")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: name, Reason: "RolloutStuck",
		Message: fmt.Sprintf("ProgressDeadlineExceeded (%d/%d available)", d.Status.AvailableReplicas, valueOr(d.Spec.Replicas, 1)),
		LogURL:  url, Rung: string(RungAuto)})
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
	for _, ns := range watchedNamespaces(ctx, deps) {
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

// serviceGrace is how long a new Service may have no ready endpoints before
// it is reported: the pods behind a fresh Service are usually still starting.
const serviceGrace = 2 * time.Minute

// CheckServiceEndpoints reports Services with a selector and no ready
// endpoint, which drop every request. It starts from the Services, not the
// Endpoints objects, so a selector that matches no pod is reported too
// (ISS-034), and counts ready addresses in EndpointSlices (ISS-035).
// Must be called only by the leader.
func CheckServiceEndpoints(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		svcs, err := deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "services", ns)
			continue
		}
		ready, err := readyEndpointsByService(ctx, deps, ns)
		if err != nil {
			countAPIError(err, "endpointslices", ns)
			continue
		}
		var pods []corev1.Pod
		podsRead := false
		for i := range svcs.Items {
			svc := &svcs.Items[i]
			if len(svc.Spec.Selector) == 0 || svc.Spec.Type == corev1.ServiceTypeExternalName ||
				now.Sub(svc.CreationTimestamp.Time) < serviceGrace || ready[svc.Name] > 0 {
				continue
			}
			if !podsRead {
				list, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					countAPIError(err, "pods", ns)
					break
				}
				pods, podsRead = list.Items, true
			}
			report(ctx, deps, serviceFinding(svc, pods))
		}
	}
}

// readyEndpointsByService counts ready addresses per Service from the
// EndpointSlices in ns. A nil ready condition means ready (discovery/v1).
func readyEndpointsByService(ctx context.Context, deps *Deps, ns string) (map[string]int, error) {
	slices, err := deps.Client.DiscoveryV1().EndpointSlices(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	ready := map[string]int{}
	for i := range slices.Items {
		s := &slices.Items[i]
		svc := s.Labels[discoveryv1.LabelServiceName]
		for _, ep := range s.Endpoints {
			if len(ep.Addresses) > 0 && (ep.Conditions.Ready == nil || *ep.Conditions.Ready) {
				ready[svc] += len(ep.Addresses)
			}
		}
	}
	return ready, nil
}

// serviceFinding explains why a Service has no ready endpoint: no pod
// matches its selector (with the pods that come closest), or pods match and
// none is ready.
func serviceFinding(svc *corev1.Service, pods []corev1.Pod) finding {
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	f := finding{Reason: "NoEndpoints", Namespace: svc.Namespace, Workload: "service/" + svc.Name,
		Severity: eventsvc.SevCritical, Rung: RungGuided, Details: []string{fmt.Sprintf("Selector: `%s`", sel)}}
	var matching []string
	for i := range pods {
		if sel.Matches(labels.Set(pods[i].Labels)) {
			matching = append(matching, fmt.Sprintf("`%s` (%s)", pods[i].Name, podState(&pods[i])))
		}
	}
	if len(matching) == 0 {
		f.Summary = "the selector matches no pod, so every request to the Service fails"
		f.Details = append(f.Details, closestPods(svc.Spec.Selector, pods)...)
		f.Fix = fmt.Sprintf("correct the Service selector or the pod labels; compare with `kubectl get pods -n %s --show-labels`", svc.Namespace)
		return f
	}
	sort.Strings(matching)
	if len(matching) > 5 {
		matching = append(matching[:5], fmt.Sprintf("and %d more", len(matching)-5))
	}
	f.Summary = fmt.Sprintf("%d pod(s) match the selector but none is ready, so every request fails", len(matching))
	f.Details = append(f.Details, "Matching pods: "+strings.Join(matching, ", "))
	f.Fix = fmt.Sprintf("find why the pods are not ready (readiness probe, crash, pending): `kubectl describe pods -n %s -l %s`", svc.Namespace, sel)
	return f
}

// closestPods names up to three pods that share the most selector labels,
// with the values they have instead, so a typo in a label is obvious.
func closestPods(selector map[string]string, pods []corev1.Pod) []string {
	type near struct {
		name  string
		score int
		diff  []string
	}
	var best []near
	for i := range pods {
		n := near{name: pods[i].Name}
		for k, want := range selector {
			got, ok := pods[i].Labels[k]
			switch {
			case ok && got == want:
				n.score++
			case ok:
				n.diff = append(n.diff, fmt.Sprintf("%s=%s", k, got))
			}
		}
		if n.score > 0 || len(n.diff) > 0 {
			sort.Strings(n.diff)
			best = append(best, n)
		}
	}
	sort.Slice(best, func(a, b int) bool {
		if best[a].score != best[b].score {
			return best[a].score > best[b].score
		}
		return best[a].name < best[b].name
	})
	var out []string
	for i := 0; i < len(best) && i < 3; i++ {
		line := fmt.Sprintf("Closest pod: `%s` matches %d of %d selector labels", best[i].name, best[i].score, len(selector))
		if len(best[i].diff) > 0 {
			line += "; it has " + strings.Join(best[i].diff, ", ")
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		out = []string{"No pod in the namespace carries any of the selector's labels"}
	}
	return out
}

// podState is a short phase and readiness for a pod.
func podState(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return string(p.Status.Phase) + ", ready"
		}
	}
	return string(p.Status.Phase) + ", not ready"
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

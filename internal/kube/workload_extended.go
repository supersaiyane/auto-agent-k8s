package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// stuckFor is how long a pod must stay not ready before a StatefulSet or
// DaemonSet counts as stuck rather than rolling (phase 12 audit).
const stuckFor = 5 * time.Minute

// CheckStatefulSetStuck reports a StatefulSet whose rollout or scale-up has
// stopped on one pod: the lowest-ordinal pod that is not ready, and has not
// been for stuckFor. With ordered startup every later pod waits for it.
func CheckStatefulSetStuck(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		stss, err := deps.Client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "statefulsets", ns)
			continue
		}
		for i := range stss.Items {
			sts := &stss.Items[i]
			if sts.Status.ReadyReplicas >= valueOr(sts.Spec.Replicas, 1) {
				continue
			}
			pods, ok := ownedPods(ctx, deps, ns, sts.Spec.Selector)
			if !ok {
				continue
			}
			if blocker, since := lowestNotReady(pods, now); blocker != nil && since >= stuckFor {
				report(ctx, deps, statefulSetFinding(sts, blocker, since))
			}
		}
	}
}

func statefulSetFinding(sts *appsv1.StatefulSet, blocker *corev1.Pod, since time.Duration) finding {
	f := finding{Reason: "StatefulSetStuck", Namespace: sts.Namespace, Workload: "statefulset/" + sts.Name, Pod: blocker.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided,
		Summary: fmt.Sprintf("%d/%d ready; pod `%s` has not been ready for %s", sts.Status.ReadyReplicas,
			valueOr(sts.Spec.Replicas, 1), blocker.Name, since.Round(time.Minute)),
		Details: []string{"State: " + waitingState(blocker)},
		Fix:     fmt.Sprintf("find why `%s` is not ready: `kubectl describe pod %s -n %s`", blocker.Name, blocker.Name, sts.Namespace),
	}
	if sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		f.Details = append(f.Details, "Ordered startup: every higher ordinal waits for this pod")
	}
	return f
}

// CheckDaemonSetMissing reports a DaemonSet with pods that have not been
// ready on their node for stuckFor, and, once its rollout is complete, nodes
// where no pod could be scheduled.
func CheckDaemonSetMissing(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		dss, err := deps.Client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "daemonsets", ns)
			continue
		}
		for i := range dss.Items {
			ds := &dss.Items[i]
			st := ds.Status
			if st.NumberReady >= st.DesiredNumberScheduled {
				continue
			}
			pods, ok := ownedPods(ctx, deps, ns, ds.Spec.Selector)
			if !ok {
				continue
			}
			var stuck, stuckNames []string
			for j := range pods {
				if since, ready := notReadyFor(&pods[j], now); !ready && since >= stuckFor {
					stuck = append(stuck, fmt.Sprintf("`%s` on `%s` (%s)", pods[j].Name, pods[j].Spec.NodeName, waitingState(&pods[j])))
					stuckNames = append(stuckNames, pods[j].Name)
				}
			}
			sort.Strings(stuck)
			sort.Strings(stuckNames)
			rolled := ds.Generation == st.ObservedGeneration && st.UpdatedNumberScheduled >= st.DesiredNumberScheduled
			unscheduled := st.DesiredNumberScheduled - st.CurrentNumberScheduled
			if len(stuck) == 0 && (!rolled || unscheduled <= 0) {
				continue // still rolling out or scheduling: not stuck
			}
			f := finding{Reason: "DaemonSetMissing", Namespace: ns, Workload: "daemonset/" + ds.Name,
				Severity: eventsvc.SevWarning, Rung: RungGuided,
				Summary: fmt.Sprintf("%d of %d nodes lack a ready pod", st.DesiredNumberScheduled-st.NumberReady, st.DesiredNumberScheduled),
				Fix:     fmt.Sprintf("describe the stuck pods (`kubectl describe pod <name> -n %s`); deleting one recreates it on its node", ns)}
			if len(stuck) > 0 {
				f.Details = append(f.Details, "Not ready for "+stuckFor.String()+" or more: "+strings.Join(stuck, ", "))
				f.Target, f.Proposal = RungApprove, proposeDeleteStuck(deps, ds, stuckNames)
			}
			if rolled && unscheduled > 0 {
				f.Details = append(f.Details, fmt.Sprintf("%d node(s) have no pod at all: check taints, tolerations and node selectors", unscheduled))
			}
			report(ctx, deps, f)
		}
	}
}

// ownedPods lists the pods a workload's selector matches; a failed read is
// counted and reported as not ok.
func ownedPods(ctx context.Context, deps *Deps, ns string, sel *metav1.LabelSelector) ([]corev1.Pod, bool) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil || selector.Empty() {
		return nil, false
	}
	list, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		countAPIError(err, "pods", ns)
		return nil, false
	}
	return list.Items, true
}

// notReadyFor reports whether a pod is ready and, if not, for how long.
func notReadyFor(p *corev1.Pod, now time.Time) (time.Duration, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			if c.Status == corev1.ConditionTrue {
				return 0, true
			}
			return now.Sub(c.LastTransitionTime.Time), false
		}
	}
	return now.Sub(p.CreationTimestamp.Time), false
}

// lowestNotReady returns the not-ready pod with the lowest StatefulSet
// ordinal and how long it has not been ready.
func lowestNotReady(pods []corev1.Pod, now time.Time) (*corev1.Pod, time.Duration) {
	var best *corev1.Pod
	var bestOrd int
	var since time.Duration
	for i := range pods {
		d, ready := notReadyFor(&pods[i], now)
		if ready {
			continue
		}
		ord := ordinal(pods[i].Name)
		if best == nil || ord < bestOrd {
			best, bestOrd, since = &pods[i], ord, d
		}
	}
	return best, since
}

// ordinal is the number after the last dash of a StatefulSet pod name.
func ordinal(name string) int {
	n, err := strconv.Atoi(name[strings.LastIndex(name, "-")+1:])
	if err != nil {
		return 1 << 30
	}
	return n
}

// waitingState names why a pod is not ready: a container's waiting reason,
// or its phase.
func waitingState(p *corev1.Pod) string {
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.Name + ": " + cs.State.Waiting.Reason
		}
	}
	return string(p.Status.Phase)
}

// CheckHPAIssues detects HPAs at max replicas or unable to scale.
func CheckHPAIssues(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		hpas, err := deps.Client.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "horizontalpodautoscalers", ns)
			continue
		}
		for i := range hpas.Items {
			checkHPA(ctx, deps, &hpas.Items[i], now)
		}
	}
}

// hpaLimitedFor is how long an HPA must be capped before it is reported.
const hpaLimitedFor = 15 * time.Minute

// checkHPA reports an HPA held at its maximum while load asks for more,
// and one that cannot compute its metrics (PLAN-002 10.15, ISS-046).
func checkHPA(ctx context.Context, deps *Deps, hpa *autoscalingv2.HorizontalPodAutoscaler, now time.Time) {
	target := strings.ToLower(hpa.Spec.ScaleTargetRef.Kind) + "/" + hpa.Spec.ScaleTargetRef.Name
	max := hpa.Spec.MaxReplicas
	fixedSize := hpa.Spec.MinReplicas != nil && *hpa.Spec.MinReplicas >= max
	if c := hpaCondition(hpa, autoscalingv2.ScalingLimited); c != nil && !fixedSize &&
		c.Status == corev1.ConditionTrue && c.Reason == "TooManyReplicas" && now.Sub(c.LastTransitionTime.Time) >= hpaLimitedFor {
		ceiling := deps.Policy().MaxReplicas
		proposed := proposeMaxReplicas(max, ceiling)
		fix := fmt.Sprintf("raise maxReplicas from %d to %d (half again, within the policy ceiling of %d), or make each replica handle more load", max, proposed, ceiling)
		f := finding{
			Reason: "HPAMaxedOut", Namespace: hpa.Namespace, Workload: target,
			Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: hpa.Name,
			Summary: fmt.Sprintf("autoscaler `%s` held at maxReplicas %d for %s while load asks for more",
				hpa.Name, max, now.Sub(c.LastTransitionTime.Time).Round(time.Minute)),
			Details: []string{fmt.Sprintf("Current %d, desired %d. %s", hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas, c.Message)},
			Fix:     fix,
		}
		if proposed <= max {
			f.Fix = fmt.Sprintf("maxReplicas is already at the policy ceiling of %d; make each replica handle more load, or raise the ceiling deliberately", ceiling)
		} else {
			f.Target, f.Proposal = RungApprove, proposeHPAMax(deps, hpa, target, proposed)
		}
		report(ctx, deps, f)
	}
	if c := hpaCondition(hpa, autoscalingv2.ScalingActive); c != nil && c.Status == corev1.ConditionFalse && c.Reason != "ScalingDisabled" {
		report(ctx, deps, finding{
			Reason: "HPAScalingFailed", Namespace: hpa.Namespace, Workload: target,
			Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: hpa.Name,
			Summary: fmt.Sprintf("autoscaler `%s` cannot compute its metrics", hpa.Name),
			Details: []string{fmt.Sprintf("%s: %s", c.Reason, c.Message)},
			Fix:     "check that metrics-server (or the custom metrics adapter) is running and that the target pods set resource requests",
		})
	}
}

// proposeMaxReplicas suggests half again the current maximum, never above
// the policy ceiling; a ceiling of zero means none.
func proposeMaxReplicas(max, ceiling int32) int32 {
	p := max + (max+1)/2
	if ceiling > 0 && p > ceiling {
		p = ceiling
	}
	return p
}

func hpaCondition(h *autoscalingv2.HorizontalPodAutoscaler, t autoscalingv2.HorizontalPodAutoscalerConditionType) *autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range h.Status.Conditions {
		if h.Status.Conditions[i].Type == t {
			return &h.Status.Conditions[i]
		}
	}
	return nil
}

// missedLookback is how far back a missed-schedule event is still news.
const missedLookback = time.Hour

// CheckCronJobMissed reports a CronJob the controller says missed a start
// (events MissSchedule or TooManyMissedTimes), whatever its concurrency
// policy (phase 12 audit: it only looked when the policy was Forbid and a
// job was active).
func CheckCronJobMissed(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		crons, err := deps.Client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "cronjobs", ns)
			continue
		}
		for i := range crons.Items {
			cj := &crons.Items[i]
			if cj.Spec.Suspend != nil && *cj.Spec.Suspend {
				continue
			}
			for _, ev := range listObjectEvents(ctx, deps.Client, ns, cj.Name) {
				if ev.InvolvedObject.Kind != "CronJob" || ev.InvolvedObject.Name != cj.Name ||
					(ev.Reason != "MissSchedule" && ev.Reason != "TooManyMissedTimes") ||
					now.Sub(eventTime(&ev)) > missedLookback {
					continue
				}
				report(ctx, deps, finding{Reason: "CronJobMissed", Namespace: ns, Workload: "cronjob/" + cj.Name,
					Severity: eventsvc.SevWarning, Rung: RungGuided, Target: RungApprove, Proposal: proposeRunNow(deps, cj, now),
					Summary: "missed a scheduled start: " + ev.Message,
					Details: []string{fmt.Sprintf("Active jobs: %d, concurrency policy %s", len(cj.Status.Active), cj.Spec.ConcurrencyPolicy)},
					Fix: fmt.Sprintf("if the previous run is still going, it blocks this one; set startingDeadlineSeconds so a late start still runs, "+
						"or run it now: `kubectl create job --from=cronjob/%s %s-manual -n %s`", cj.Name, cj.Name, ns)})
				break
			}
		}
	}
}

// eventTime is when an event last happened, whichever field is set.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// pausedFor is how long a Deployment must stay paused to count as
// forgotten; it is then reminded once a day, not every dedup window.
const pausedFor = time.Hour

// CheckDeploymentPaused reports a Deployment paused for pausedFor or more.
func CheckDeploymentPaused(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		deploys, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "deployments", ns)
			continue
		}
		for i := range deploys.Items {
			d := &deploys.Items[i]
			if !d.Spec.Paused {
				continue
			}
			since := pausedSince(d)
			if since.IsZero() || now.Sub(since) < pausedFor ||
				!deps.Dedup.CheckFor(dedupKey(ns, d.Name, "DeploymentPausedDaily"), 24*time.Hour) {
				continue
			}
			report(ctx, deps, finding{Reason: "DeploymentPaused", Namespace: ns, Workload: "deployment/" + d.Name,
				Severity: eventsvc.SevInfo, Rung: RungGuided, Target: RungApprove, Proposal: proposeResume(deps, d),
				Summary: fmt.Sprintf("paused for %s: no rollout happens, including fixes", now.Sub(since).Round(time.Minute)),
				Fix:     fmt.Sprintf("if the pause was not meant to last: `kubectl rollout resume deployment/%s -n %s`", d.Name, ns)})
		}
	}
}

// pausedSince is when the Deployment controller saw the pause, from the
// Progressing condition with reason DeploymentPaused.
func pausedSince(d *appsv1.Deployment) time.Time {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "DeploymentPaused" {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// CheckReplicaSetFailure reports a ReplicaSet the controller cannot create
// pods for (condition ReplicaFailure), whether it has some pods or none
// (phase 12 audit: it only looked at ReplicaSets with zero pods).
func CheckReplicaSetFailure(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		rss, err := deps.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "replicasets", ns)
			continue
		}
		for i := range rss.Items {
			rs := &rss.Items[i]
			for _, c := range rs.Status.Conditions {
				if c.Type != appsv1.ReplicaSetReplicaFailure || c.Status != corev1.ConditionTrue {
					continue
				}
				report(ctx, deps, finding{Reason: "ReplicaSetFailure", Namespace: ns, Workload: "replicaset/" + rs.Name,
					Severity: eventsvc.SevCritical, Rung: RungGuided,
					Summary: fmt.Sprintf("cannot create pods: %d of %d exist", rs.Status.Replicas, valueOr(rs.Spec.Replicas, 1)),
					Details: []string{fmt.Sprintf("%s: %s", c.Reason, c.Message)},
					Fix:     "the message names the cause: usually a ResourceQuota, a LimitRange or an admission webhook refusing the pod"})
			}
		}
	}
}

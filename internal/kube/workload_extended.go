package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// CheckStatefulSetStuck detects StatefulSets stuck in ordered ready (pod N waiting for N-1).
func CheckStatefulSetStuck(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		stss, err := deps.Client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "statefulsets", ns)
			continue
		}
		for _, sts := range stss.Items {
			desired := int32(1)
			if sts.Spec.Replicas != nil {
				desired = *sts.Spec.Replicas
			}
			if sts.Status.ReadyReplicas < desired && sts.Status.UpdatedReplicas < desired {
				// Check if it's been stuck for >5 min
				for _, c := range sts.Status.Conditions {
					if c.Type == "Progressing" {
						continue
					}
				}
				key := dedupKey(ns, sts.Name, "StatefulSetStuck")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*StatefulSetStuck* `%s/%s`: %d/%d ready, %d updated\n",
					ns, sts.Name, sts.Status.ReadyReplicas, desired, sts.Status.UpdatedReplicas)
				msg += "_Check_: previous pod may not be Ready (ordered startup). Check pod events.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: sts.Name, Reason: "StatefulSetStuck",
					Message: fmt.Sprintf("%d/%d ready", sts.Status.ReadyReplicas, desired)})
				obs.IncidentsTotal.WithLabelValues("StatefulSetStuck", ns, sts.Name).Inc()
			}
		}
	}
}

// CheckDaemonSetMissing detects DaemonSets not running on all expected nodes.
func CheckDaemonSetMissing(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		dss, err := deps.Client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "daemonsets", ns)
			continue
		}
		for _, ds := range dss.Items {
			if ds.Status.DesiredNumberScheduled > ds.Status.NumberReady {
				missing := ds.Status.DesiredNumberScheduled - ds.Status.NumberReady
				key := dedupKey(ns, ds.Name, "DaemonSetMissing")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*DaemonSetMissing* `%s/%s`: %d pods not scheduled/ready (desired=%d, ready=%d)\n",
					ns, ds.Name, missing, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady)
				if ds.Status.NumberMisscheduled > 0 {
					msg += fmt.Sprintf("Misscheduled: %d\n", ds.Status.NumberMisscheduled)
				}
				msg += "_Check_: node taints, tolerations, resource availability.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: ds.Name, Reason: "DaemonSetMissing",
					Message: fmt.Sprintf("%d pods missing", missing)})
				obs.IncidentsTotal.WithLabelValues("DaemonSetMissing", ns, ds.Name).Inc()
			}
		}
	}
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
		if proposed <= max {
			fix = fmt.Sprintf("maxReplicas is already at the policy ceiling of %d; make each replica handle more load, or raise the ceiling deliberately", ceiling)
		}
		report(ctx, deps, finding{
			Reason: "HPAMaxedOut", Namespace: hpa.Namespace, Workload: target,
			Severity: eventsvc.SevWarning, Rung: RungGuided, Target: RungApprove, Subject: hpa.Name,
			Summary: fmt.Sprintf("autoscaler `%s` held at maxReplicas %d for %s while load asks for more",
				hpa.Name, max, now.Sub(c.LastTransitionTime.Time).Round(time.Minute)),
			Details: []string{fmt.Sprintf("Current %d, desired %d. %s", hpa.Status.CurrentReplicas, hpa.Status.DesiredReplicas, c.Message)},
			Fix:     fix,
		})
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

// CheckCronJobMissed detects CronJobs that missed their schedule.
func CheckCronJobMissed(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		crons, err := deps.Client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "cronjobs", ns)
			continue
		}
		for _, cj := range crons.Items {
			// Check if too many active jobs (concurrency issue)
			if cj.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(cj.Status.Active) > 0 {
				if cj.Status.LastScheduleTime != nil {
					// Check for missed schedules by looking at events
					events := collectEvents(ctx, deps.Client, ns, cj.Name)
					for _, ev := range events {
						if containsStr(ev, "MissSchedule") || containsStr(ev, "missed") {
							key := dedupKey(ns, cj.Name, "CronJobMissed")
							if !deps.Dedup.Check(key) {
								break
							}
							msg := fmt.Sprintf("*CronJobMissed* `%s/%s` missed schedule\n", ns, cj.Name)
							msg += fmt.Sprintf("Active jobs: %d, Policy: %s\n", len(cj.Status.Active), cj.Spec.ConcurrencyPolicy)
							msg += "_Check_: previous job still running, or increase startingDeadlineSeconds.\n"
							deps.Slack.Post(msg)
							recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
								Namespace: ns, Workload: cj.Name, Reason: "CronJobMissed",
								Message: fmt.Sprintf("Missed schedule, %d active", len(cj.Status.Active))})
							obs.IncidentsTotal.WithLabelValues("CronJobMissed", ns, cj.Name).Inc()
							break
						}
					}
				}
			}
		}
	}
}

// CheckDeploymentPaused detects deployments someone paused and forgot.
func CheckDeploymentPaused(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		deploys, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "deployments", ns)
			continue
		}
		for _, d := range deploys.Items {
			if d.Spec.Paused {
				key := dedupKey(ns, d.Name, "DeploymentPaused")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*DeploymentPaused* `%s/%s` is paused: no rollouts will happen\n", ns, d.Name)
				msg += "_Check_: was this intentional? Resume: `kubectl rollout resume deploy/%s -n %s`\n"
				msg = fmt.Sprintf(msg, d.Name, ns)
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevInfo,
					Namespace: ns, Workload: d.Name, Reason: "DeploymentPaused",
					Message: "Deployment is paused"})
				obs.IncidentsTotal.WithLabelValues("DeploymentPaused", ns, d.Name).Inc()
			}
		}
	}
}

// CheckReplicaSetFailure detects ReplicaSets that can't create pods.
func CheckReplicaSetFailure(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		rss, err := deps.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "replicasets", ns)
			continue
		}
		for _, rs := range rss.Items {
			desired := int32(0)
			if rs.Spec.Replicas != nil {
				desired = *rs.Spec.Replicas
			}
			if desired > 0 && rs.Status.ReadyReplicas == 0 && rs.Status.Replicas == 0 {
				// RS wants pods but has none, likely blocked by quota or admission
				key := dedupKey(ns, rs.Name, "ReplicaSetFailure")
				if !deps.Dedup.Check(key) {
					continue
				}
				for _, c := range rs.Status.Conditions {
					if c.Type == appsv1.ReplicaSetReplicaFailure {
						msg := fmt.Sprintf("*ReplicaSetFailure* `%s/%s` cannot create pods\n", ns, rs.Name)
						msg += fmt.Sprintf("Reason: %s: %s\n", c.Reason, c.Message)
						msg += "_Check_: ResourceQuota, admission webhooks, or scheduling constraints.\n"
						deps.Slack.Post(msg)
						recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
							Namespace: ns, Workload: rs.Name, Reason: "ReplicaSetFailure", Message: c.Message})
						obs.IncidentsTotal.WithLabelValues("ReplicaSetFailure", ns, rs.Name).Inc()
						break
					}
				}
			}
		}
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && findSubstr(s, substr))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

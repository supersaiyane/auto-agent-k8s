package kube

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckStatefulSetStuck detects StatefulSets stuck in ordered ready (pod N waiting for N-1).
func CheckStatefulSetStuck(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		stss, err := deps.Client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
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
				msg := fmt.Sprintf("*StatefulSetStuck* `%s/%s` — %d/%d ready, %d updated\n",
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
	for ns := range deps.Policy.NamespaceAllow {
		dss, err := deps.Client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ds := range dss.Items {
			if ds.Status.DesiredNumberScheduled > ds.Status.NumberReady {
				missing := ds.Status.DesiredNumberScheduled - ds.Status.NumberReady
				key := dedupKey(ns, ds.Name, "DaemonSetMissing")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*DaemonSetMissing* `%s/%s` — %d pods not scheduled/ready (desired=%d, ready=%d)\n",
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
	for ns := range deps.Policy.NamespaceAllow {
		hpas, err := deps.Client.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, hpa := range hpas.Items {
			// HPA maxed out
			if hpa.Status.CurrentReplicas >= hpa.Spec.MaxReplicas && hpa.Status.CurrentReplicas > 0 {
				key := dedupKey(ns, hpa.Name, "HPAMaxedOut")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*HPAMaxedOut* `%s/%s` at max replicas (%d/%d)\n",
					ns, hpa.Name, hpa.Status.CurrentReplicas, hpa.Spec.MaxReplicas)
				msg += fmt.Sprintf("Target: %s\n", hpa.Spec.ScaleTargetRef.Name)
				msg += "_Warning_: workload at maximum scale. Consider increasing maxReplicas or optimizing.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: hpa.Spec.ScaleTargetRef.Name, Reason: "HPAMaxedOut",
					Message: fmt.Sprintf("At max replicas %d", hpa.Spec.MaxReplicas)})
				obs.IncidentsTotal.WithLabelValues("HPAMaxedOut", ns, hpa.Name).Inc()
			}

			// HPA unable to compute metrics
			for _, c := range hpa.Status.Conditions {
				if c.Type == "ScalingActive" && c.Status == "False" {
					key := dedupKey(ns, hpa.Name, "HPAScalingFailed")
					if !deps.Dedup.Check(key) {
						continue
					}
					msg := fmt.Sprintf("*HPAScalingFailed* `%s/%s` cannot compute metrics\n", ns, hpa.Name)
					msg += fmt.Sprintf("Reason: %s — %s\n", c.Reason, c.Message)
					msg += "_Check_: metrics-server running, resource metrics available.\n"
					deps.Slack.Post(msg)
					recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
						Namespace: ns, Workload: hpa.Name, Reason: "HPAScalingFailed", Message: c.Message})
					obs.IncidentsTotal.WithLabelValues("HPAScalingFailed", ns, hpa.Name).Inc()
				}
			}
		}
	}
}

// CheckCronJobMissed detects CronJobs that missed their schedule.
func CheckCronJobMissed(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		crons, err := deps.Client.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
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
	for ns := range deps.Policy.NamespaceAllow {
		deploys, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, d := range deploys.Items {
			if d.Spec.Paused {
				key := dedupKey(ns, d.Name, "DeploymentPaused")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*DeploymentPaused* `%s/%s` is paused — no rollouts will happen\n", ns, d.Name)
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
	for ns := range deps.Policy.NamespaceAllow {
		rss, err := deps.Client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, rs := range rss.Items {
			desired := int32(0)
			if rs.Spec.Replicas != nil {
				desired = *rs.Spec.Replicas
			}
			if desired > 0 && rs.Status.ReadyReplicas == 0 && rs.Status.Replicas == 0 {
				// RS wants pods but has none — likely blocked by quota or admission
				key := dedupKey(ns, rs.Name, "ReplicaSetFailure")
				if !deps.Dedup.Check(key) {
					continue
				}
				for _, c := range rs.Status.Conditions {
					if c.Type == appsv1.ReplicaSetReplicaFailure {
						msg := fmt.Sprintf("*ReplicaSetFailure* `%s/%s` cannot create pods\n", ns, rs.Name)
						msg += fmt.Sprintf("Reason: %s — %s\n", c.Reason, c.Message)
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

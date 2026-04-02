package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
)

// CheckFailedJobs scans for failed Jobs and CronJobs and alerts.
// Must be called only by the leader.
func CheckFailedJobs(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		jobs, err := deps.Client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.V(3).Infof("jobs: failed to list jobs in %s: %v", ns, err)
			continue
		}

		for _, job := range jobs.Items {
			if !isJobFailed(&job) {
				continue
			}

			key := dedupKey(ns, job.Name, "JobFailed")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("JobFailed").Inc()
				continue
			}

			handleFailedJob(ctx, deps, &job)
		}
	}
}

func isJobFailed(job *batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == "True" {
			return true
		}
	}
	return false
}

func handleFailedJob(ctx context.Context, deps *Deps, job *batchv1.Job) {
	ns := job.Namespace
	name := job.Name
	klog.Infof("handler: failed Job detected %s/%s", ns, name)

	// Collect logs from the most recent pod
	events := collectEvents(ctx, deps.Client, ns, name)
	var logs string
	pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", name),
	})
	if err == nil && len(pods.Items) > 0 {
		lastPod := pods.Items[len(pods.Items)-1]
		if len(lastPod.Spec.Containers) > 0 {
			logs = getLastLogs(ctx, deps.Client, ns, lastPod.Name, lastPod.Spec.Containers[0].Name, 50)
		}
	}

	url, err := persistLogBundle(ctx, deps.Sink, ns, name, name, "", "",
		"JobFailed", "Job failed", logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("job", "storage").Inc()
	}

	// Determine failure reason
	failReason := "unknown"
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed {
			failReason = cond.Message
			break
		}
	}

	// Check if it's a CronJob child
	cronJobName := ""
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "CronJob" {
			cronJobName = ref.Name
			break
		}
	}

	msg := fmt.Sprintf("*JobFailed*: `%s/%s`", ns, name)
	if cronJobName != "" {
		msg += fmt.Sprintf(" (CronJob: `%s`)", cronJobName)
	}
	msg += fmt.Sprintf("\nReason: %s\nFailed pods: %d\nSaved: `%s`\n", failReason, job.Status.Failed, url)

	// In fix mode, clean up old failed jobs (> 1 hour) to prevent accumulation
	if deps.Policy.Mode == policy.Fix && cronJobName != "" {
		cleaned := cleanupOldFailedJobs(ctx, deps, ns, cronJobName)
		if cleaned > 0 {
			msg += fmt.Sprintf("_Action_: cleaned up %d old failed jobs for CronJob `%s`.\n", cleaned, cronJobName)
		}
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Job Failed", logs+"\n"+strings.Join(events, "\n"))

	if err := deps.Slack.Post(msg); err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("job", "slack").Inc()
	}
	obs.IncidentsTotal.WithLabelValues("JobFailed", ns, name).Inc()
}

// cleanupOldFailedJobs removes failed jobs older than 1 hour for a given CronJob.
func cleanupOldFailedJobs(ctx context.Context, deps *Deps, ns, cronJobName string) int {
	jobs, err := deps.Client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}

	cleaned := 0
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, job := range jobs.Items {
		if !isJobFailed(&job) {
			continue
		}
		// Check if owned by same CronJob
		ownedByCron := false
		for _, ref := range job.OwnerReferences {
			if ref.Kind == "CronJob" && ref.Name == cronJobName {
				ownedByCron = true
				break
			}
		}
		if !ownedByCron {
			continue
		}
		if job.CreationTimestamp.Time.Before(cutoff) {
			bg := metav1.DeletePropagationBackground
			if err := deps.Client.BatchV1().Jobs(ns).Delete(ctx, job.Name, metav1.DeleteOptions{
				PropagationPolicy: &bg,
			}); err == nil {
				cleaned++
				obs.ActionsTotal.WithLabelValues("delete_job", ns, cronJobName).Inc()
			}
		}
	}
	return cleaned
}

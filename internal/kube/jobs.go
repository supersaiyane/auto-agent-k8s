package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// CheckFailedJobs scans for failed Jobs and CronJobs and alerts.
// Must be called only by the leader.
func CheckFailedJobs(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy().NamespaceAllow {
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
	if err != nil {
		countAPIError(err, "pods", ns) // ISS-045: no longer dropped
	} else if len(pods.Items) > 0 {
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

	// Determine failure reason (PLAN-002 10.7): the condition reason, such
	// as BackoffLimitExceeded, decides the guidance.
	failReason, condReason := "unknown", ""
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed {
			condReason = cond.Reason
			failReason = strings.TrimPrefix(cond.Reason+": "+cond.Message, ": ")
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
	if fix := jobFailureFix(job, condReason); fix != "" {
		msg += "_Fix_: " + fix + "\n"
	}

	// In fix mode, clean up old failed jobs (> 1 hour) to prevent accumulation
	if cronJobName != "" {
		msg += cleanupOldFailedJobs(ctx, deps, ns, cronJobName)
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Job Failed", logs+"\n"+strings.Join(events, "\n"))

	if err := deps.Slack.Post(msg); err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("job", "slack").Inc()
	}
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: "job/" + name, Reason: "JobFailed", Message: failReason, LogURL: url, Rung: string(RungGuided)})
	obs.IncidentsTotal.WithLabelValues("JobFailed", ns, name).Inc()
}

// jobFailureFix is the guidance for a Job's failure reason.
func jobFailureFix(job *batchv1.Job, reason string) string {
	switch reason {
	case "BackoffLimitExceeded":
		limit := int32(6) // the Kubernetes default
		if job.Spec.BackoffLimit != nil {
			limit = *job.Spec.BackoffLimit
		}
		return fmt.Sprintf("the pods failed more than backoffLimit (%d) times; the logs above are from the last attempt. Raise backoffLimit only if the failures are transient", limit)
	case "DeadlineExceeded":
		if d := job.Spec.ActiveDeadlineSeconds; d != nil {
			return fmt.Sprintf("the job ran longer than activeDeadlineSeconds (%d); make it faster or raise the deadline", *d)
		}
		return "the job ran longer than its activeDeadlineSeconds"
	case "PodFailurePolicy":
		return "a podFailurePolicy rule failed the job on purpose; the message names the rule"
	}
	return ""
}

// cleanupOldFailedJobs removes failed jobs older than 1 hour for a given
// CronJob, as one gated action. Returns the gate's message for Slack.
func cleanupOldFailedJobs(ctx context.Context, deps *Deps, ns, cronJobName string) string {
	jobs, err := deps.Client.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(3).Infof("jobs: failed to list jobs in %s: %v", ns, err)
		return ""
	}

	var old []string
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, job := range jobs.Items {
		if isJobFailed(&job) && ownedByCronJob(&job, cronJobName) && job.CreationTimestamp.Time.Before(cutoff) {
			old = append(old, job.Name)
		}
	}
	if len(old) == 0 {
		return ""
	}

	_, msg := applyMutation(ctx, deps, mutation{
		Namespace: ns, Workload: cronJobName, Reason: "JobFailed", ActionType: "delete_job",
		SuccessMsg: fmt.Sprintf("cleaned up %d old failed jobs for CronJob `%s`", len(old), cronJobName),
		SuggestMsg: fmt.Sprintf("delete %d failed jobs older than 1h for CronJob `%s`", len(old), cronJobName),
		Apply: func() error {
			bg := metav1.DeletePropagationBackground
			var errs []error
			for _, name := range old {
				if err := deps.Client.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{
					PropagationPolicy: &bg,
				}); err != nil {
					errs = append(errs, fmt.Errorf("delete job %s/%s: %w", ns, name, err))
				}
			}
			return errors.Join(errs...)
		},
	})
	return msg
}

func ownedByCronJob(job *batchv1.Job, cronJobName string) bool {
	for _, ref := range job.OwnerReferences {
		if ref.Kind == "CronJob" && ref.Name == cronJobName {
			return true
		}
	}
	return false
}

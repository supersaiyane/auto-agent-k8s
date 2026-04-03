package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// additionalPodReasons lists extra waiting reasons we detect beyond the main handlers.
var additionalPodReasons = map[string]struct{}{
	"RunContainerError":     {},
	"ContainerCannotRun":    {},
	"PostStartHookError":    {},
	"PreStopHookError":      {},
	"InvalidImageName":      {},
	"ErrImageNeverPull":     {},
	"StartError":            {},
}

// handleAdditionalPodIssue handles pod waiting states not covered by the main handlers.
func handleAdditionalPodIssue(ctx context.Context, deps *Deps, pod *corev1.Pod, cname, reason, message string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: %s on %s/%s (container: %s)", reason, ns, name, cname)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 30)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, _ := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		reason, message, logs, events)

	sev := eventsvc.SevWarning
	if reason == "RunContainerError" || reason == "ContainerCannotRun" || reason == "InvalidImageName" {
		sev = eventsvc.SevCritical
	}

	msg := fmt.Sprintf("*%s* on `%s/%s` (container: `%s`)\n", reason, ns, name, cname)
	if message != "" {
		msg += fmt.Sprintf("Detail: %s\n", message)
	}
	msg += fmt.Sprintf("Saved: `%s`\n", url)

	switch reason {
	case "RunContainerError":
		msg += "_Check_: container entrypoint/command, binary exists in image, file permissions.\n"
	case "ContainerCannotRun":
		msg += "_Check_: security context (runAsUser/readOnlyRootFilesystem), volume mount permissions.\n"
	case "PostStartHookError":
		msg += "_Check_: postStart lifecycle hook command and timeout.\n"
	case "InvalidImageName":
		msg += "_Check_: image reference format — must be registry/repo:tag.\n"
	case "ErrImageNeverPull":
		msg += "_Check_: image is pre-loaded on the node, or change imagePullPolicy from Never.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, reason, logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, reason, ns, wl, name, msg, string(sev))
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: sev,
		Namespace: ns, Workload: wl, Pod: name, Node: pod.Spec.NodeName,
		Reason: reason, Message: message, LogURL: url})
	createTicket(ctx, deps, fmt.Sprintf("%s-%s-%s", strings.ToLower(reason), ns, wl),
		fmt.Sprintf("%s: %s/%s", reason, ns, wl), msg)
	obs.IncidentsTotal.WithLabelValues(reason, ns, wl).Inc()
}

// CheckDeadlineExceeded detects pods that exceeded their activeDeadlineSeconds.
func CheckDeadlineExceeded(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Failed",
		})
		if err != nil {
			continue
		}
		for _, pod := range pods.Items {
			if pod.Status.Reason != "DeadlineExceeded" {
				continue
			}
			key := dedupKey(ns, pod.Name, "DeadlineExceeded")
			if !deps.Dedup.Check(key) {
				continue
			}
			wl := ownerName(&pod)
			msg := fmt.Sprintf("*DeadlineExceeded* pod `%s/%s` exceeded its activeDeadlineSeconds\n", ns, pod.Name)
			if pod.Spec.ActiveDeadlineSeconds != nil {
				msg += fmt.Sprintf("Deadline: %ds\n", *pod.Spec.ActiveDeadlineSeconds)
			}
			msg += "_Check_: increase activeDeadlineSeconds or investigate why pod is slow.\n"

			deps.Slack.Post(msg)
			fireAlert(ctx, deps, "DeadlineExceeded", ns, wl, pod.Name, msg, "warning")
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
				Namespace: ns, Workload: wl, Pod: pod.Name, Reason: "DeadlineExceeded",
				Message: "Pod exceeded activeDeadlineSeconds"})
			obs.IncidentsTotal.WithLabelValues("DeadlineExceeded", ns, wl).Inc()
		}
	}
}

// CheckEphemeralStorageFull detects pods evicted due to ephemeral storage.
func CheckEphemeralStorageFull(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "status.phase=Failed",
		})
		if err != nil {
			continue
		}
		for _, pod := range pods.Items {
			if pod.Status.Reason != "Evicted" {
				continue
			}
			if !strings.Contains(pod.Status.Message, "ephemeral-storage") {
				continue
			}
			key := dedupKey(ns, ownerName(&pod), "EphemeralStorageFull")
			if !deps.Dedup.Check(key) {
				continue
			}
			wl := ownerName(&pod)
			msg := fmt.Sprintf("*EphemeralStorageFull* pod `%s/%s` evicted\n%s\n", ns, pod.Name, pod.Status.Message)
			msg += "_Check_: reduce log output, clean temp files, or increase ephemeral-storage limit.\n"
			deps.Slack.Post(msg)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
				Namespace: ns, Workload: wl, Pod: pod.Name, Reason: "EphemeralStorageFull",
				Message: pod.Status.Message})
			obs.IncidentsTotal.WithLabelValues("EphemeralStorageFull", ns, wl).Inc()
		}
	}
}

// isAdditionalPodReason returns true if this is a reason we handle in the extended handler.
func isAdditionalPodReason(reason string) bool {
	_, ok := additionalPodReasons[reason]
	return ok
}

// podAge returns age since creation.
func podAge(pod *corev1.Pod) time.Duration {
	return time.Since(pod.CreationTimestamp.Time)
}

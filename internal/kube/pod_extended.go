package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// additionalPodReasons lists extra waiting reasons we detect beyond the main handlers.
var additionalPodReasons = map[string]struct{}{
	"RunContainerError":  {},
	"ContainerCannotRun": {},
	"PostStartHookError": {},
	"PreStopHookError":   {},
	"InvalidImageName":   {},
	"ErrImageNeverPull":  {},
	"StartError":         {},
}

// handleAdditionalPodIssue handles pod waiting states not covered by the main handlers.
func handleAdditionalPodIssue(ctx context.Context, deps *Deps, pod *corev1.Pod, cname, reason, message string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: %s on %s/%s (container: %s)", reason, ns, name, cname)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 30)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		reason, message, logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("logbundle", "storage").Inc()
	}

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
		msg += "_Check_: image reference format: must be registry/repo:tag.\n"
	case "ErrImageNeverPull":
		msg += "_Check_: image is pre-loaded on the node, or change imagePullPolicy from Never.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, reason, logs+"\n"+strings.Join(events, "\n"))
	postIncident(deps, ns, pod.Labels, msg)
	fireAlert(ctx, deps, reason, ns, wl, name, msg, string(sev))
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: sev,
		Namespace: ns, Workload: wl, Pod: name, Node: pod.Spec.NodeName,
		Reason: reason, Message: message, LogURL: url})
	createTicket(ctx, deps, ns, pod.Labels, fmt.Sprintf("%s-%s-%s", strings.ToLower(reason), ns, wl),
		fmt.Sprintf("%s: %s/%s", reason, ns, wl), msg)
	obs.IncidentsTotal.WithLabelValues(reason, ns, wl).Inc()
}

// terminalPodWindow is how long one failed pod stays reported once: a
// Failed pod does not change until it is deleted (phase 12 audit).
const terminalPodWindow = 24 * time.Hour

// failedPods lists the Failed pods in ns; a failed read is counted.
func failedPods(ctx context.Context, deps *Deps, ns string) []corev1.Pod {
	pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{FieldSelector: "status.phase=Failed"})
	if err != nil {
		countAPIError(err, "pods", ns)
		return nil
	}
	return pods.Items
}

// CheckDeadlineExceeded reports a pod killed because it ran past its
// activeDeadlineSeconds, once per pod.
func CheckDeadlineExceeded(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		for _, pod := range failedPods(ctx, deps, ns) {
			if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "DeadlineExceeded" ||
				!deps.Dedup.CheckFor(dedupKey(ns, string(pod.UID), "DeadlineExceeded"), terminalPodWindow) {
				continue
			}
			f := finding{Reason: "DeadlineExceeded", Namespace: ns, Workload: ownerName(&pod), Pod: pod.Name, Node: pod.Spec.NodeName,
				Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: pod.Name,
				Summary: "the pod ran past its activeDeadlineSeconds and was stopped",
				Fix:     "make the work finish sooner, or raise activeDeadlineSeconds if the run time is expected"}
			if d := pod.Spec.ActiveDeadlineSeconds; d != nil {
				f.Details = []string{fmt.Sprintf("Deadline: %ds", *d)}
			}
			report(ctx, deps, f)
		}
	}
}

// ephemeralMarkers are the phrases the kubelet uses when it evicts a pod for
// local storage: node pressure ("low on resource: ephemeral-storage") and a
// container or pod over its limit ("local ephemeral storage limit",
// "ephemeral local storage usage"). The audit found only the first matched.
var ephemeralMarkers = []string{"ephemeral-storage", "ephemeral storage", "ephemeral local storage"}

// CheckEphemeralStorageFull reports pods evicted for local storage, once per
// pod and at most once per workload per dedup window.
func CheckEphemeralStorageFull(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		for _, pod := range failedPods(ctx, deps, ns) {
			if pod.Status.Phase != corev1.PodFailed || pod.Status.Reason != "Evicted" || !mentionsEphemeral(pod.Status.Message) ||
				!deps.Dedup.CheckFor(dedupKey(ns, string(pod.UID), "EphemeralStorageFull"), terminalPodWindow) {
				continue
			}
			report(ctx, deps, finding{Reason: "EphemeralStorageFull", Namespace: ns, Workload: ownerName(&pod), Pod: pod.Name,
				Node: pod.Spec.NodeName, Severity: eventsvc.SevWarning, Rung: RungGuided,
				Summary: "evicted for local (ephemeral) storage",
				Details: []string{pod.Status.Message},
				Fix:     "write less to the container filesystem and emptyDir (logs, temp files), or raise the ephemeral-storage request and limit"})
		}
	}
}

func mentionsEphemeral(msg string) bool {
	m := strings.ToLower(msg)
	for _, k := range ephemeralMarkers {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
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

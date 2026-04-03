package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
)

// handleInitContainerFailure handles Init:Error and Init:CrashLoopBackOff.
func handleInitContainerFailure(ctx context.Context, deps *Deps, pod *corev1.Pod, initName, reason string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: init container failure on %s/%s (init: %s, reason: %s)", ns, name, initName, reason)

	logs := getLastLogs(ctx, deps.Client, ns, name, initName, 50)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, initName, pod.Spec.NodeName,
		"InitContainerFailed", reason, logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("initcontainer", "storage").Inc()
	}

	msg := fmt.Sprintf("*InitContainerFailed* on `%s/%s` (init: `%s`, reason: `%s`)\nSaved: `%s`\n",
		ns, name, initName, reason, url)
	msg += "_Check_: init container command, dependencies, volumes, and network access.\n"

	if deps.Policy.Mode == policy.Fix {
		crdPol := effectivePolicy(deps, ns, pod.Labels)
		if policyAllowsAction(crdPol) && deps.Limiter.Allow() {
			if checkBreaker(ctx, deps, ns, wl) {
				if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
					klog.Warningf("handler: failed to delete pod %s/%s: %v", ns, name, err)
				} else {
					msg += "_Action_: deleted pod to retry init containers.\n"
					obs.ActionsTotal.WithLabelValues("delete_pod", ns, wl).Inc()
				}
			}
		}
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Init container failure: "+reason, logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "InitContainerFailed", ns, wl, name, msg, "warning")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "InitContainerFailed",
		Message: fmt.Sprintf("Init container %s: %s", initName, reason), LogURL: url})
	createTicket(ctx, deps, fmt.Sprintf("init-%s-%s", ns, wl), fmt.Sprintf("InitContainerFailed: %s/%s", ns, wl), msg)
	obs.IncidentsTotal.WithLabelValues("InitContainerFailed", ns, wl).Inc()
}

// handleConfigError handles CreateContainerConfigError (missing ConfigMap/Secret refs).
func handleConfigError(ctx context.Context, deps *Deps, pod *corev1.Pod, cname, reason string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: config error on %s/%s (container: %s)", ns, name, cname)

	events := collectEvents(ctx, deps.Client, ns, name)
	url, _ := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"ConfigError", reason, "", events)

	msg := fmt.Sprintf("*CreateContainerConfigError* on `%s/%s` (container: `%s`)\nReason: %s\nSaved: `%s`\n",
		ns, name, cname, reason, url)
	msg += "_Check_: referenced ConfigMaps and Secrets exist in namespace `" + ns + "`.\n"

	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "ConfigError", ns, wl, name, msg, "critical")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: wl, Pod: name, Reason: "ConfigError", Message: reason, LogURL: url})
	createTicket(ctx, deps, fmt.Sprintf("config-%s-%s", ns, wl), fmt.Sprintf("ConfigError: %s/%s", ns, wl), msg)
	obs.IncidentsTotal.WithLabelValues("ConfigError", ns, wl).Inc()
}

// handleRestartStorm detects containers restarting rapidly (>5 restarts, not yet in BackOff).
func handleRestartStorm(ctx context.Context, deps *Deps, pod *corev1.Pod, cname string, restartCount int32) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: restart storm on %s/%s (container: %s, restarts: %d)", ns, name, cname, restartCount)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 30)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, _ := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"RestartStorm", fmt.Sprintf("Rapid restarts: %d", restartCount), logs, events)

	msg := fmt.Sprintf("*RestartStorm* on `%s/%s` (container: `%s`, restarts: %d)\nSaved: `%s`\n",
		ns, name, cname, restartCount, url)
	msg += "_Warning_: container restarting rapidly. Likely heading toward CrashLoopBackOff.\n"

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Container restart storm", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "RestartStorm", ns, wl, name, msg, "warning")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "RestartStorm",
		Message: fmt.Sprintf("Container %s: %d restarts", cname, restartCount), LogURL: url})
	obs.IncidentsTotal.WithLabelValues("RestartStorm", ns, wl).Inc()
}

// --- Helpers used by extended handlers ---

// checkBreaker wraps circuit breaker check and fires alert if tripped.
func checkBreaker(ctx context.Context, deps *Deps, ns, wl string) bool {
	if deps.Breaker == nil {
		return true
	}
	if !deps.Breaker.RecordAndCheck(ns, wl) {
		if deps.AlertManager != nil {
			deps.AlertManager.FireCircuitBreaker(ctx, ns, wl)
		}
		obs.HandlerErrorsTotal.WithLabelValues("circuit_breaker", "tripped").Inc()
		return false
	}
	return true
}

// fireAlert sends an alert to Alertmanager if configured.
func fireAlert(ctx context.Context, deps *Deps, reason, ns, wl, pod, message, severity string) {
	if deps.AlertManager != nil {
		deps.AlertManager.FireIncident(ctx, reason, ns, wl, pod, message, severity)
	}
}

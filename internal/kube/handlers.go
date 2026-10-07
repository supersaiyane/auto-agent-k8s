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
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func handleCrashLoop(ctx context.Context, deps *Deps, pod *corev1.Pod, cname string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: CrashLoopBackOff detected on %s/%s (container: %s)", ns, name, cname)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 50)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"CrashLoopBackOff", "CrashLoopBackOff detected", logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("crashloop", "storage").Inc()
	}

	msg := fmt.Sprintf("*CrashLoopBackOff* on `%s/%s` (container: `%s`)\nLogs+events saved: `%s`\n", ns, name, cname, url)
	msg += tryFixAction(ctx, deps, ns, wl, name, pod.Labels, "CrashLoopBackOff", "delete_pod",
		func() error { return deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}) },
		"deleted pod to clear backoff (controller will recreate)",
		"delete pod to clear backoff",
	)

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Pod CrashLoopBackOff", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "CrashLoopBackOff", ns, wl, name, msg, "critical")
	createTicket(ctx, deps, fmt.Sprintf("crashloop-%s-%s", ns, wl), fmt.Sprintf("CrashLoopBackOff: %s/%s", ns, wl), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: wl, Pod: name, Node: pod.Spec.NodeName,
		Reason: "CrashLoopBackOff", Message: fmt.Sprintf("Container %s crash-looping", cname), LogURL: url})
	obs.IncidentsTotal.WithLabelValues("CrashLoopBackOff", ns, wl).Inc()
}

func handleImagePullBackOff(ctx context.Context, deps *Deps, pod *corev1.Pod, cname string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: ImagePullBackOff detected on %s/%s (container: %s)", ns, name, cname)

	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"ImagePullBackOff", "Image pull failure", "", events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("imagepull", "storage").Inc()
	}

	image := imageOf(pod, cname)
	cause := classifyPull(append([]string{waitingMessage(pod, cname)}, events...)...)
	msg := fmt.Sprintf("*ImagePullBackOff* on `%s/%s` (container: `%s`, image: `%s`)\nSaved: `%s`\n", ns, name, cname, image, url)
	msg += fmt.Sprintf("Cause: %s\n_Fix_: %s\n", cause.label, pullFix(cause, pod, image))
	rung := RungAuto
	if cause.retry {
		msg += tryFixAction(ctx, deps, ns, wl, name, pod.Labels, "ImagePullBackOff", "delete_pod",
			func() error { return deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}) },
			"deleted pod to retry image pull",
			"delete pod to retry image pull",
		)
	} else {
		// Retrying cannot help here, and on a rate limit it adds pulls (ISS-044).
		rung = RungGuided
		msg += "_No retry_: deleting the pod would not change this cause.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "ImagePullBackOff", strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "ImagePullBackOff", ns, wl, name, msg, "critical")
	createTicket(ctx, deps, fmt.Sprintf("imagepull-%s-%s", ns, wl), fmt.Sprintf("ImagePullBackOff: %s/%s image=%s", ns, wl, image), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: wl, Pod: name, Reason: "ImagePullBackOff",
		Message: fmt.Sprintf("Cannot pull image %s: %s", image, cause.label), LogURL: url, Rung: string(rung)})
	obs.IncidentsTotal.WithLabelValues("ImagePullBackOff", ns, wl).Inc()
}

func handleOOM(ctx context.Context, deps *Deps, pod *corev1.Pod, cname string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: OOMKilled detected on %s/%s (container: %s)", ns, name, cname)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 20)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"OOMKilled", "Container OOMKilled", logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("oom", "storage").Inc()
	}

	var memLimit string
	for _, c := range pod.Spec.Containers {
		if c.Name == cname {
			if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				memLimit = lim.String()
			}
			break
		}
	}

	msg := fmt.Sprintf("*OOMKilled* on `%s/%s` (container: `%s`, current limit: `%s`)\nSaved: `%s`\n",
		ns, name, cname, memLimit, url)

	// GitOps PR for memory bump
	if deps.GitOps != nil && deps.Policy().Mode == policy.Fix {
		bumpPct := 20
		crdPolicies := deps.CRDStore.Match(ns, pod.Labels)
		for _, cp := range crdPolicies {
			if cp.BumpMemoryPercent > 0 {
				bumpPct = cp.BumpMemoryPercent
				break
			}
		}
		prTitle := fmt.Sprintf("Bump memory for %s/%s by %d%%", ns, wl, bumpPct)
		// Generate actual patch content
		patchContent, changeDesc := GenerateMemoryBumpContent("", ns, wl, cname, memLimit, bumpPct)
		prBody := fmt.Sprintf("Container `%s` was OOMKilled with limit `%s`.\n\nChange: %s\n\nIncident log: `%s`",
			cname, memLimit, changeDesc, url)
		prURL, err := deps.GitOps.OpenPR(ctx, integrations.GitOpsChange{
			Title:    prTitle,
			Body:     prBody,
			Branch:   fmt.Sprintf("auto-agent/oom-%s-%s-%d", ns, sanitizeBranch(wl), time.Now().Unix()),
			FilePath: fmt.Sprintf("patches/%s/%s-memory-bump.yaml", ns, deploymentName(wl)),
			Content:  []byte(patchContent),
		})
		if err != nil {
			klog.Warningf("handler: failed to open OOM PR: %v", err)
			obs.HandlerErrorsTotal.WithLabelValues("oom", "gitops").Inc()
			msg += fmt.Sprintf("_GitOps_: failed to open PR: %v\n", err)
		} else {
			msg += fmt.Sprintf("_GitOps_: opened PR to bump memory: %s\n", prURL)
			auditAction(deps, "open_pr", ns, wl, name, "OOMKilled", "success", prURL)
		}
	} else {
		msg += "_Recommend_: increase memory limit by 20-50%% via GitOps PR.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Container OOMKilled", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "OOMKilled", ns, wl, name, msg, "critical")
	createTicket(ctx, deps, fmt.Sprintf("oom-%s-%s", ns, wl), fmt.Sprintf("OOMKilled: %s/%s limit=%s", ns, wl, memLimit), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: wl, Pod: name, Node: pod.Spec.NodeName,
		Reason: "OOMKilled", Message: fmt.Sprintf("Container %s killed (limit: %s)", cname, memLimit), LogURL: url})
	obs.IncidentsTotal.WithLabelValues("OOMKilled", ns, wl).Inc()
}

func handleNotReady(ctx context.Context, deps *Deps, pod *corev1.Pod, cname string) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	klog.Infof("handler: NotReady detected on %s/%s (container: %s)", ns, name, cname)

	logs := getLastLogs(ctx, deps.Client, ns, name, cname, 30)
	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, cname, pod.Spec.NodeName,
		"NotReady", "Container running but not ready", logs, events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("notready", "storage").Inc()
	}

	msg := fmt.Sprintf("*NotReady* on `%s/%s` (container: `%s`): running but failing readiness probe\nSaved: `%s`\n",
		ns, name, cname, url)
	msg += "_Check_: readiness probe endpoint, application startup, and dependencies.\n"
	msg += tryFixAction(ctx, deps, ns, wl, name, pod.Labels, "NotReady", "delete_pod",
		func() error { return deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}) },
		"deleted pod to restart (readiness probe failing >3m)",
		"delete pod to restart",
	)

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Pod NotReady", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "NotReady", ns, wl, name, msg, "warning")
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "NotReady",
		Message: fmt.Sprintf("Container %s failing readiness probe", cname), LogURL: url})
	obs.IncidentsTotal.WithLabelValues("NotReady", ns, wl).Inc()
}

func handlePending(ctx context.Context, deps *Deps, pod *corev1.Pod) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
	if pod.Spec.NodeName == "" {
		return // unschedulable: CheckPodStates on the leader names the constraint (ISS-043)
	}
	if len(volumeFailures(listObjectEvents(ctx, deps.Client, ns, name))) > 0 {
		return // volume failures: CheckPodStates names the volume (PLAN-002 10.3)
	}
	klog.Infof("handler: Pending pod detected %s/%s (>5m)", ns, name)

	events := collectEvents(ctx, deps.Client, ns, name)
	url, err := persistLogBundle(ctx, deps.Sink, ns, wl, name, "", pod.Spec.NodeName,
		"Pending", "Pod stuck in Pending", "", events)
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("pending", "storage").Inc()
	}

	reason := "unknown"
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
			reason = cond.Message
			break
		}
	}

	msg := fmt.Sprintf("*Pending* pod `%s/%s` (>5 minutes)\nReason: %s\nSaved: `%s`\n", ns, name, reason, url)
	if strings.Contains(reason, "Insufficient") {
		msg += "_Diagnosis_: cluster lacks resources. Consider scaling node pool or adjusting resource requests.\n"
	} else if strings.Contains(reason, "node(s) didn't match") {
		msg += "_Diagnosis_: no nodes match scheduling constraints. Check nodeSelector, affinity, and taints.\n"
	} else if strings.Contains(reason, "persistentvolumeclaim") {
		msg += "_Diagnosis_: PVC not bound. Check StorageClass and available PersistentVolumes.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Pod stuck Pending", strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	fireAlert(ctx, deps, "Pending", ns, wl, name, msg, "warning")
	createTicket(ctx, deps, fmt.Sprintf("pending-%s-%s", ns, wl), fmt.Sprintf("Pending: %s/%s: %s", ns, wl, reason), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "Pending", Message: reason, LogURL: url})
	obs.IncidentsTotal.WithLabelValues("Pending", ns, wl).Inc()
}

// tryFixAction sends a remediation for an unhealthy workload through the
// mutation gate and, when it was applied, starts recovery verification.
// Returns a message string describing what happened.
func tryFixAction(ctx context.Context, deps *Deps, ns, wl, pod string, labels map[string]string,
	reason, actionType string, action func() error, successMsg, suggestMsg string) string {

	outcome, msg := applyMutation(ctx, deps, mutation{
		Namespace: ns, Workload: wl, Pod: pod, Labels: labels,
		Reason: reason, ActionType: actionType,
		SuccessMsg: successMsg, SuggestMsg: suggestMsg,
		Apply: action,
	})
	if outcome != gateApplied {
		return msg
	}

	// Record action taken: FixTracker will verify if workload actually recovered
	if deps.FixTracker != nil {
		deps.FixTracker.RecordAction(ns, wl, pod, reason, actionType)
	}
	// Record as "action-taken" (pending verification), NOT as "fix" yet
	recordEvent(deps, eventsvc.Event{
		Type: eventsvc.Action, Severity: eventsvc.SevInfo,
		Namespace: ns, Workload: wl, Pod: pod,
		Reason:  reason,
		Message: fmt.Sprintf("Action taken: %s (verifying recovery...)", successMsg),
		Action:  actionType,
	})
	return fmt.Sprintf("_Action_: %s (verifying recovery...).\n", successMsg)
}

// createTicket creates or updates a ticket if ticketing is configured.
func createTicket(ctx context.Context, deps *Deps, key, title, body string) {
	if deps.Ticketer == nil {
		return
	}
	_, err := deps.Ticketer.CreateOrUpdate(ctx, key, integrations.Ticket{
		Title:  title,
		Body:   body,
		Labels: []string{"auto-agent", "kubernetes"},
	})
	if err != nil {
		klog.Warningf("handler: ticket creation failed for %s: %v", key, err)
		obs.HandlerErrorsTotal.WithLabelValues("ticket", "create").Inc()
	}
}

// recordEvent records an event to the in-memory ring buffer for the UI dashboard.
func recordEvent(deps *Deps, evt eventsvc.Event) {
	if deps.Recorder != nil {
		deps.Recorder.Record(evt)
	}
}

// sanitizeBranch makes a string safe for git branch names.
func sanitizeBranch(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	return r.Replace(s)
}

// pullCause is why an image pull failed, and whether retrying can help
// (PLAN-002 10.16, ISS-044).
type pullCause struct {
	kind, label string
	retry       bool
}

// pullCauses is checked in order; the first phrase found decides.
var pullCauses = []struct {
	phrases []string
	cause   pullCause
}{
	{[]string{"toomanyrequests", "429 too many requests", "rate limit"},
		pullCause{"ratelimited", "registry rate limit (toomanyrequests)", false}},
	{[]string{"unauthorized", "authentication required", "no basic auth credentials", "403 forbidden", "access denied", "denied:"},
		pullCause{"unauthorized", "registry refused the credentials (unauthorized)", false}},
	{[]string{"manifest unknown", "not found", "name unknown"},
		pullCause{"notfound", "image or tag does not exist", false}},
	{[]string{"i/o timeout", "no such host", "connection refused", "tls handshake timeout", "dial tcp"},
		pullCause{"network", "node cannot reach the registry", true}},
}

func classifyPull(texts ...string) pullCause {
	all := strings.ToLower(strings.Join(texts, "\n"))
	for _, c := range pullCauses {
		for _, p := range c.phrases {
			if strings.Contains(all, p) {
				return c.cause
			}
		}
	}
	return pullCause{"unknown", "not recognised from the pull error", true}
}

func pullFix(c pullCause, pod *corev1.Pod, image string) string {
	switch c.kind {
	case "ratelimited":
		return "authenticate pulls with an imagePullSecret, pull through a mirror or cache, or pin images so nodes reuse cached layers; the agent does not retry, because each retry is another pull against the limit"
	case "unauthorized":
		secrets := make([]string, 0, len(pod.Spec.ImagePullSecrets))
		for _, s := range pod.Spec.ImagePullSecrets {
			secrets = append(secrets, s.Name)
		}
		have := "none are set on the pod"
		if len(secrets) > 0 {
			have = "the pod uses `" + strings.Join(secrets, "`, `") + "`"
		}
		return fmt.Sprintf("give the pod an imagePullSecret with a valid token for this registry in namespace `%s` (%s), or attach one to its service account", pod.Namespace, have)
	case "notfound":
		return fmt.Sprintf("check the reference `%s` and that the tag was pushed to the registry", image)
	case "network":
		return fmt.Sprintf("check DNS and egress from node `%s` to the registry; the agent retries once the path is back", pod.Spec.NodeName)
	}
	return "check the image name, tag, registry credentials and network access to the registry"
}

// waitingMessage is the kubelet's message for a waiting container.
func waitingMessage(pod *corev1.Pod, cname string) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == cname && cs.State.Waiting != nil {
			return cs.State.Waiting.Message
		}
	}
	return ""
}

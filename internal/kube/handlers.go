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
	"github.com/yourorg/auto-agent/internal/integrations"
	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
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

	if deps.Policy.Mode == policy.Fix {
		if !deps.Limiter.Allow() {
			obs.RateLimitedTotal.Inc()
			msg += "_Action_: rate limited, skipping pod deletion.\n"
		} else {
			if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				klog.Warningf("handler: failed to delete pod %s/%s: %v", ns, name, err)
				obs.HandlerErrorsTotal.WithLabelValues("crashloop", "delete").Inc()
			} else {
				msg += "_Action_: deleted pod to clear backoff (controller will recreate).\n"
				obs.ActionsTotal.WithLabelValues("delete_pod", ns, wl).Inc()
			}
		}
	} else if deps.Policy.Mode == policy.Suggest {
		msg += "_Suggest_: delete pod to clear backoff.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Pod CrashLoopBackOff", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	createTicket(ctx, deps, fmt.Sprintf("crashloop-%s-%s", ns, wl),
		fmt.Sprintf("CrashLoopBackOff: %s/%s", ns, wl), msg)
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
	msg := fmt.Sprintf("*ImagePullBackOff* on `%s/%s` (container: `%s`, image: `%s`)\nSaved: `%s`\n", ns, name, cname, image, url)
	msg += "_Check_: image name, tag, registry credentials (ImagePullSecret), and network access to registry.\n"

	if deps.Policy.Mode == policy.Fix {
		if !deps.Limiter.Allow() {
			obs.RateLimitedTotal.Inc()
		} else {
			if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				klog.Warningf("handler: failed to delete pod %s/%s: %v", ns, name, err)
				obs.HandlerErrorsTotal.WithLabelValues("imagepull", "delete").Inc()
			} else {
				msg += "_Action_: deleted pod to retry image pull.\n"
				obs.ActionsTotal.WithLabelValues("delete_pod", ns, wl).Inc()
			}
		}
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "ImagePullBackOff", strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	createTicket(ctx, deps, fmt.Sprintf("imagepull-%s-%s", ns, wl),
		fmt.Sprintf("ImagePullBackOff: %s/%s image=%s", ns, wl, image), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
		Namespace: ns, Workload: wl, Pod: name, Reason: "ImagePullBackOff",
		Message: fmt.Sprintf("Cannot pull image %s", image), LogURL: url})
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

	// Open GitOps PR to bump memory if available
	if deps.GitOps != nil && deps.Policy.Mode == policy.Fix {
		bumpPct := 20
		crdPolicies := deps.CRDStore.Match(ns, pod.Labels)
		for _, cp := range crdPolicies {
			if cp.BumpMemoryPercent > 0 {
				bumpPct = cp.BumpMemoryPercent
				break
			}
		}
		prTitle := fmt.Sprintf("Bump memory for %s/%s by %d%%", ns, wl, bumpPct)
		prBody := fmt.Sprintf("Container `%s` was OOMKilled with limit `%s`.\n\nRecommend increasing by %d%%.\n\nIncident log: `%s`",
			cname, memLimit, bumpPct, url)
		prURL, err := deps.GitOps.OpenPR(ctx, integrations.GitOpsChange{
			Title:  prTitle,
			Body:   prBody,
			Branch: fmt.Sprintf("auto-agent/oom-%s-%s-%d", ns, sanitizeBranch(wl), time.Now().Unix()),
		})
		if err != nil {
			klog.Warningf("handler: failed to open OOM PR: %v", err)
			obs.HandlerErrorsTotal.WithLabelValues("oom", "gitops").Inc()
			msg += fmt.Sprintf("_GitOps_: failed to open PR: %v\n", err)
		} else {
			msg += fmt.Sprintf("_GitOps_: opened PR to bump memory: %s\n", prURL)
		}
	} else {
		msg += "_Recommend_: increase memory limit by 20-50%% via GitOps PR.\n"
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Container OOMKilled", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	createTicket(ctx, deps, fmt.Sprintf("oom-%s-%s", ns, wl),
		fmt.Sprintf("OOMKilled: %s/%s limit=%s", ns, wl, memLimit), msg)
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

	msg := fmt.Sprintf("*NotReady* on `%s/%s` (container: `%s`) — running but failing readiness probe\nSaved: `%s`\n",
		ns, name, cname, url)
	msg += "_Check_: readiness probe endpoint, application startup, and dependencies.\n"

	if deps.Policy.Mode == policy.Fix {
		if !deps.Limiter.Allow() {
			obs.RateLimitedTotal.Inc()
		} else {
			if err := deps.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
				klog.Warningf("handler: failed to delete not-ready pod %s/%s: %v", ns, name, err)
				obs.HandlerErrorsTotal.WithLabelValues("notready", "delete").Inc()
			} else {
				msg += "_Action_: deleted pod to restart (readiness probe failing >3m).\n"
				obs.ActionsTotal.WithLabelValues("delete_pod", ns, wl).Inc()
			}
		}
	}

	msg += deps.LLM.DiagnoseWithFallback(ctx, "Pod NotReady", logs+"\n"+strings.Join(events, "\n"))
	deps.Slack.Post(msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "NotReady",
		Message: fmt.Sprintf("Container %s failing readiness probe", cname), LogURL: url})
	obs.IncidentsTotal.WithLabelValues("NotReady", ns, wl).Inc()
}

func handlePending(ctx context.Context, deps *Deps, pod *corev1.Pod) {
	ns, name := pod.Namespace, pod.Name
	wl := ownerName(pod)
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
	createTicket(ctx, deps, fmt.Sprintf("pending-%s-%s", ns, wl),
		fmt.Sprintf("Pending: %s/%s — %s", ns, wl, reason), msg)
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
		Namespace: ns, Workload: wl, Pod: name, Reason: "Pending",
		Message: reason, LogURL: url})
	obs.IncidentsTotal.WithLabelValues("Pending", ns, wl).Inc()
}

// recordEvent records an event to the in-memory ring buffer for the UI dashboard.
func recordEvent(deps *Deps, evt eventsvc.Event) {
	if deps.Recorder != nil {
		deps.Recorder.Record(evt)
	}
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

// sanitizeBranch makes a string safe for git branch names.
func sanitizeBranch(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	return r.Replace(s)
}

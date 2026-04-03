package kube

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/storage"
)

// dedupKey builds a deduplication key at the WORKLOAD level (not pod level).
// This prevents delete-recreate-delete storms when a controller recreates pods.
func dedupKey(ns, workloadOrName, reason string) string {
	return fmt.Sprintf("%s/%s/%s", ns, workloadOrName, reason)
}

// ownerName returns "kind/name" of the controlling owner, or "pod/name".
func ownerName(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return fmt.Sprintf("%s/%s", strings.ToLower(o.Kind), o.Name)
		}
	}
	return "pod/" + p.Name
}

// imageOf returns the image for a named container in the pod.
func imageOf(p *corev1.Pod, cname string) string {
	for _, c := range p.Spec.Containers {
		if c.Name == cname {
			return c.Image
		}
	}
	return ""
}

// hasAnnotation returns true if the pod has the specified annotation key.
func hasAnnotation(p *corev1.Pod, key string) bool {
	if key == "" || p.Annotations == nil {
		return false
	}
	_, ok := p.Annotations[key]
	return ok
}

// getLastLogs fetches the tail N lines of logs for a container.
func getLastLogs(ctx context.Context, kc kubernetes.Interface, ns, pod, container string, lines int64) string {
	opts := &corev1.PodLogOptions{Container: container, TailLines: &lines}
	req := kc.CoreV1().Pods(ns).GetLogs(pod, opts)
	r, err := req.Stream(ctx)
	if err != nil {
		klog.V(3).Infof("logs: failed to stream %s/%s/%s: %v", ns, pod, container, err)
		return fmt.Sprintf("(log fetch error: %v)", err)
	}
	defer r.Close()
	// Limit read to 64KB
	b, err := io.ReadAll(io.LimitReader(r, 64*1024))
	if err != nil {
		return fmt.Sprintf("(log read error: %v)", err)
	}
	return string(b)
}

// collectEvents fetches Kubernetes events for a specific object, limited to 100.
func collectEvents(ctx context.Context, kc kubernetes.Interface, ns, name string) []string {
	evs, err := kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("involvedObject.name", name).String(),
		Limit:         100,
	})
	if err != nil {
		klog.V(3).Infof("events: failed to list for %s/%s: %v", ns, name, err)
		return nil
	}
	out := make([]string, 0, len(evs.Items))
	for _, e := range evs.Items {
		out = append(out, fmt.Sprintf("[%s] %s %s: %s", e.LastTimestamp.Format(time.RFC3339), e.Type, e.Reason, e.Message))
	}
	return out
}

// persistLogBundle saves logs + events to the configured storage sink.
func persistLogBundle(ctx context.Context, sink storage.Sink, ns, workload, pod, container, node, reason, message, logs string, events []string) (string, error) {
	key := storage.BuildKey(ns, workload, pod, reason, time.Now())
	rec := &storage.Record{
		Timestamp: time.Now().UTC(),
		Namespace: ns,
		Workload:  workload,
		Pod:       pod,
		Container: container,
		Node:      node,
		Reason:    reason,
		Message:   message,
		LastLogs:  logs,
		Events:    events,
	}
	url, err := sink.Save(ctx, key, rec)
	if err != nil {
		klog.Warningf("storage: failed to persist %s: %v", key, err)
		return "", err
	}
	return url, nil
}

// parseDuration wraps time.ParseDuration with a fallback.
func parseDuration(s, fallback string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		d, _ = time.ParseDuration(fallback)
	}
	return d
}

// isCriticalPod returns true for system-critical pods that should not be evicted.
func isCriticalPod(p *corev1.Pod) bool {
	pc := p.Spec.PriorityClassName
	if strings.HasPrefix(pc, "system-") {
		return true
	}
	if strings.Contains(strings.ToLower(pc), "critical") {
		return true
	}
	if p.Namespace == "kube-system" {
		return true
	}
	return false
}

// isStatefulSetPod returns true if the pod is owned by a StatefulSet.
func isStatefulSetPod(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Kind == "StatefulSet" {
			return true
		}
	}
	return false
}

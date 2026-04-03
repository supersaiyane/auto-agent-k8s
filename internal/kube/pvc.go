package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckPendingPVCs detects PersistentVolumeClaims stuck in Pending.
// Must be called only by the leader.
func CheckPendingPVCs(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		pvcs, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.V(3).Infof("pvc: failed to list in %s: %v", ns, err)
			continue
		}
		for _, pvc := range pvcs.Items {
			if pvc.Status.Phase != corev1.ClaimPending {
				continue
			}
			// Only alert if pending > 5 minutes
			if pvc.CreationTimestamp.Time.After(metav1.Now().Add(-5 * 60e9)) {
				continue
			}

			key := dedupKey(ns, pvc.Name, "PVCPending")
			if !deps.Dedup.Check(key) {
				obs.DedupSkippedTotal.WithLabelValues("PVCPending").Inc()
				continue
			}

			klog.Infof("handler: PVC stuck pending %s/%s", ns, pvc.Name)

			events := collectEvents(ctx, deps.Client, ns, pvc.Name)
			reason := "unknown"
			for _, ev := range events {
				if len(ev) > 0 {
					reason = ev
					break
				}
			}

			scName := "<default>"
			if pvc.Spec.StorageClassName != nil {
				scName = *pvc.Spec.StorageClassName
			}
			size := "unknown"
			if req, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				size = req.String()
			}

			msg := fmt.Sprintf("*PVCPending* `%s/%s` stuck in Pending (>5 min)\n", ns, pvc.Name)
			msg += fmt.Sprintf("StorageClass: `%s`, Size: `%s`, AccessModes: %v\n", scName, size, pvc.Spec.AccessModes)
			msg += fmt.Sprintf("_Check_: StorageClass exists, provisioner is running, capacity available.\n")
			if reason != "unknown" {
				msg += fmt.Sprintf("Latest event: %s\n", reason)
			}

			deps.Slack.Post(msg)
			fireAlert(ctx, deps, "PVCPending", ns, pvc.Name, "", msg, "warning")
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
				Namespace: ns, Workload: pvc.Name, Reason: "PVCPending",
				Message: fmt.Sprintf("PVC pending: storageClass=%s size=%s", scName, size)})
			createTicket(ctx, deps, fmt.Sprintf("pvc-%s-%s", ns, pvc.Name),
				fmt.Sprintf("PVC Pending: %s/%s", ns, pvc.Name), msg)
			obs.IncidentsTotal.WithLabelValues("PVCPending", ns, pvc.Name).Inc()
		}
	}
}

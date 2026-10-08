package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// pvcPendingFor is how long a claim may stay Pending before it is reported.
const pvcPendingFor = 5 * time.Minute

// CheckPendingPVCs reports PersistentVolumeClaims stuck in Pending, with the
// newest event from the provisioner. A claim waiting for its first consumer
// (WaitForFirstConsumer binding) is pending by design and is not reported.
// Must be called only by the leader.
func CheckPendingPVCs(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		pvcs, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "persistentvolumeclaims", ns) // phase 12: was a silent continue
			continue
		}
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			if pvc.Status.Phase != corev1.ClaimPending || now.Sub(pvc.CreationTimestamp.Time) < pvcPendingFor {
				continue
			}
			latest := newestEvent(listObjectEvents(ctx, deps.Client, ns, pvc.Name), pvc.Name)
			if latest != nil && latest.Reason == "WaitForFirstConsumer" {
				continue
			}
			if f := pendingPVCFinding(pvc, latest, now); report(ctx, deps, f) {
				createTicket(ctx, deps, ns, nil, fmt.Sprintf("pvc-%s-%s", ns, pvc.Name), fmt.Sprintf("PVC Pending: %s/%s", ns, pvc.Name), f.message())
			}
		}
	}
}

func pendingPVCFinding(pvc *corev1.PersistentVolumeClaim, latest *corev1.Event, now time.Time) finding {
	class := "<default>"
	if pvc.Spec.StorageClassName != nil {
		class = *pvc.Spec.StorageClassName
	}
	size := "unknown"
	if req, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		size = req.String()
	}
	f := finding{Reason: "PVCPending", Namespace: pvc.Namespace, Workload: "pvc/" + pvc.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided,
		Summary: fmt.Sprintf("Pending for %s", now.Sub(pvc.CreationTimestamp.Time).Round(time.Minute)),
		Details: []string{fmt.Sprintf("StorageClass `%s`, size %s, access modes %v", class, size, pvc.Spec.AccessModes)},
		Fix:     fmt.Sprintf("check that the StorageClass exists, its provisioner is running and has capacity: `kubectl describe pvc %s -n %s`", pvc.Name, pvc.Namespace)}
	if latest != nil {
		f.Details = append(f.Details, fmt.Sprintf("Latest event: %s %s: %s", latest.Type, latest.Reason, latest.Message))
	}
	return f
}

// newestEvent is the most recent event about the named object; the API
// returns events in no particular order (phase 12 audit).
func newestEvent(evs []corev1.Event, name string) *corev1.Event {
	var mine []corev1.Event
	for i := range evs {
		if evs[i].InvolvedObject.Name == name {
			mine = append(mine, evs[i])
		}
	}
	if len(mine) == 0 {
		return nil
	}
	sort.Slice(mine, func(a, b int) bool { return eventTime(&mine[a]).After(eventTime(&mine[b])) })
	return &mine[0]
}

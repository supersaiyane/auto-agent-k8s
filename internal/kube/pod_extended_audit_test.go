package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func failedPod(name, reason, msg string) *corev1.Pod {
	return pod(name, func(p *corev1.Pod) {
		p.UID = types.UID("uid-" + name)
		p.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: reason, Message: msg}
	})
}

// Phase 12 audit of CheckDeadlineExceeded.
//
// Claims: a pod stopped for running past activeDeadlineSeconds.
// Condition: phase Failed, status reason DeadlineExceeded.
// Bug found: a Failed pod stays until deleted, so it was reported again
// every dedup window; now once per pod UID (terminalPodWindow).
// Known miss: a Job's own deadline is reported by CheckFailedJobs, because
// the Job controller deletes the pods instead of failing them. Rung R1.
func TestAudit_DeadlineExceeded(t *testing.T) {
	d := int64(600)
	late := failedPod("batch-x", "DeadlineExceeded", "Pod was active on the node longer than the specified deadline")
	late.Spec.ActiveDeadlineSeconds = &d
	h := newFindingHarness(t, late, failedPod("crashed", "Error", ""), pod("running", nil))
	CheckDeadlineExceeded(context.Background(), h.deps)
	CheckDeadlineExceeded(context.Background(), h.deps)
	msg := h.expect(t, "DeadlineExceeded", RungGuided, 1)[0]
	if !strings.Contains(msg, "batch-x") || !strings.Contains(msg, "Deadline: 600s") {
		t.Fatalf("message: %s", msg)
	}
	expectCounted(t, "pods", func(d *Deps) { CheckDeadlineExceeded(context.Background(), d) })
}

// Phase 12 audit of CheckEphemeralStorageFull.
//
// Claims: a pod evicted for local storage.
// Bugs found: (1) it matched only "ephemeral-storage", which appears in the
// node-pressure message, so a container over its own limit ("local ephemeral
// storage limit") or a pod over the sum ("ephemeral local storage usage") was
// never reported; (2) the evicted pod was reported again every dedup window.
// Rung R1.
func TestAudit_EphemeralStorageFull(t *testing.T) {
	pressure := failedPod("a", "Evicted", "The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi")
	container := failedPod("b", "Evicted", `Container app exceeded its local ephemeral storage limit "500Mi".`)
	podSum := failedPod("c", "Evicted", "Pod ephemeral local storage usage exceeds the total limit of containers 1Gi.")
	memory := failedPod("d", "Evicted", "The node was low on resource: memory.")
	for i, p := range []*corev1.Pod{container, podSum} {
		p.OwnerReferences = nil
		p.Name = []string{"b", "c"}[i]
	}
	h := newFindingHarness(t, pressure, container, podSum, memory)
	CheckEphemeralStorageFull(context.Background(), h.deps)
	CheckEphemeralStorageFull(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "EphemeralStorageFull", RungGuided, 3), "\n")
	for _, want := range []string{"low on resource: ephemeral-storage", "local ephemeral storage limit", "ephemeral local storage usage"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q", want)
		}
	}
	if strings.Contains(msgs, "memory") {
		t.Error("a memory eviction is not a storage finding")
	}
	expectCounted(t, "pods", func(d *Deps) { CheckEphemeralStorageFull(context.Background(), d) })
}

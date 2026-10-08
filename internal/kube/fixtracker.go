package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// FixRecord tracks an action taken and whether the workload recovered.
type FixRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	Namespace  string    `json:"namespace"`
	Workload   string    `json:"workload"`
	Pod        string    `json:"pod"`
	Reason     string    `json:"reason"`
	Action     string    `json:"action"`
	Status     string    `json:"status"` // "pending", "fixed", "not-fixed"
	VerifiedAt time.Time `json:"verifiedAt,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// HealthySince is when the workload was first seen healthy again; it
	// counts as fixed only once it stays healthy for fixStableFor (ISS-038).
	HealthySince time.Time `json:"healthySince,omitempty"`
}

const (
	fixSettleAfter = 30 * time.Second // first check this long after the action
	fixStableFor   = time.Minute      // healthy this long counts as fixed
	fixGiveUpAfter = 15 * time.Minute // not healthy by then: not fixed
)

// FixTracker monitors actions taken and verifies if the workload actually recovered.
type FixTracker struct {
	now     func() time.Time // injectable clock (PLAN-002 8.5); nil means time.Now
	mu      sync.Mutex
	pending []FixRecord
	fixed   []FixRecord
	failed  []FixRecord
	maxLen  int
}

func NewFixTracker(maxLen int) *FixTracker {
	if maxLen <= 0 {
		maxLen = 200
	}
	return &FixTracker{
		pending: make([]FixRecord, 0),
		fixed:   make([]FixRecord, 0, maxLen),
		failed:  make([]FixRecord, 0, maxLen),
		maxLen:  maxLen,
	}
}

// RecordAction adds a pending fix to track. Will be verified later.
func (ft *FixTracker) RecordAction(ns, workload, pod, reason, action string) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.pending = append(ft.pending, FixRecord{
		Timestamp: ft.clock().UTC(),
		Namespace: ns,
		Workload:  workload,
		Pod:       pod,
		Reason:    reason,
		Action:    action,
		Status:    "pending",
	})
}

// VerifyFixes checks if pending actions actually fixed the problem.
// A fix is confirmed when the workload's pods are all Running+Ready.
// Call periodically from the leader loop.
func VerifyFixes(ctx context.Context, deps *Deps) {
	if deps.FixTracker == nil {
		return
	}

	deps.FixTracker.mu.Lock()
	pending := make([]FixRecord, len(deps.FixTracker.pending))
	copy(pending, deps.FixTracker.pending)
	deps.FixTracker.mu.Unlock()

	var stillPending []FixRecord

	for _, rec := range pending {
		now := deps.FixTracker.clock()
		if now.Sub(rec.Timestamp) > fixGiveUpAfter {
			rec.Status = "not-fixed"
			rec.VerifiedAt = deps.FixTracker.clock().UTC()
			rec.Detail = "timed out: workload did not recover within 15 minutes"
			deps.FixTracker.addFailed(rec)
			klog.V(3).Infof("fixtracker: timed out %s/%s reason=%s", rec.Namespace, rec.Workload, rec.Reason)
			escalate(deps, "FixNotRecovered", rec.Namespace, rec.Workload, fmt.Sprintf("%s for %s did not bring %s back within %s",
				rec.Action, rec.Reason, rec.Workload, fixGiveUpAfter)) // once: the record leaves pending here
			continue
		}

		if now.Sub(rec.Timestamp) < fixSettleAfter {
			stillPending = append(stillPending, rec)
			continue
		}

		// One healthy sample is not enough: a crash-looping pod is Ready
		// between restarts. It must stay healthy for fixStableFor.
		healthy, detail := isWorkloadHealthy(ctx, deps, rec.Namespace, rec.Workload)
		switch {
		case !healthy:
			rec.HealthySince = time.Time{}
		case rec.HealthySince.IsZero():
			rec.HealthySince = now
		}
		if healthy && now.Sub(rec.HealthySince) >= fixStableFor {
			rec.Status = "fixed"
			rec.VerifiedAt = deps.FixTracker.clock().UTC()
			rec.Detail = detail
			deps.FixTracker.addFixed(rec)

			// Record as a verified fix event for the dashboard
			recordEvent(deps, eventsvc.Event{
				Type: eventsvc.Action, Severity: eventsvc.SevInfo,
				Namespace: rec.Namespace, Workload: rec.Workload,
				Reason: rec.Reason,
				Action: "verified-fix",
				Message: fmt.Sprintf("Fixed: %s → %s (verified healthy after %s)",
					rec.Reason, rec.Action, deps.FixTracker.clock().Sub(rec.Timestamp).Round(time.Second)),
			})
			obs.ActionsTotal.WithLabelValues("verified_fix", rec.Namespace, rec.Workload).Inc()
			klog.Infof("fixtracker: VERIFIED FIX %s/%s reason=%s action=%s recovery=%s",
				rec.Namespace, rec.Workload, rec.Reason, rec.Action, deps.FixTracker.clock().Sub(rec.Timestamp).Round(time.Second))
		} else {
			// Still not healthy, keep checking
			stillPending = append(stillPending, rec)
		}
	}

	// Actions recorded while this pass ran were appended after the snapshot;
	// keep them (phase 12 audit: they were overwritten and lost).
	deps.FixTracker.mu.Lock()
	deps.FixTracker.pending = append(stillPending, deps.FixTracker.pending[len(pending):]...)
	deps.FixTracker.mu.Unlock()
}

// isWorkloadHealthy checks if the workload's parent Deployment/StatefulSet is healthy.
// The workload name is "replicaset/name-hash", we resolve to the parent Deployment
// because after a fix, a NEW ReplicaSet is created with a different hash.
func isWorkloadHealthy(ctx context.Context, deps *Deps, ns, workload string) (bool, string) {
	// Extract deployment name from "replicaset/api-server-8446f784fd"
	deployName := resolveDeploymentName(workload)

	deploy, err := deps.Client.AppsV1().Deployments(ns).Get(ctx, deployName, metav1.GetOptions{})
	if err == nil {
		return deploymentRecovered(deploy)
	}
	if !apierrors.IsNotFound(err) {
		countAPIError(err, "deployments", ns)
		return false, "cannot read deployment: " + err.Error()
	}

	// Fallback: check pods directly by owner name match
	pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "pods", ns)
		return false, ""
	}

	matching := 0
	ready := 0
	for _, p := range pods.Items {
		if ownerName(&p) == workload || "pod/"+p.Name == workload {
			matching++
			allReady := true
			for _, cs := range p.Status.ContainerStatuses {
				if !cs.Ready || cs.State.Waiting != nil {
					allReady = false
					break
				}
			}
			if allReady && p.Status.Phase == "Running" {
				ready++
			}
		}
	}
	if matching == 0 {
		return false, "no matching pods"
	}
	if ready == matching {
		return true, fmt.Sprintf("%d/%d pods Running+Ready", ready, matching)
	}
	return false, fmt.Sprintf("%d/%d pods ready", ready, matching)
}

// deploymentRecovered reports a Deployment whose current spec has fully
// rolled out: observed, every replica updated and ready, none unavailable.
// Old ready replicas during a rollout do not count (ISS-038).
func deploymentRecovered(d *appsv1.Deployment) (bool, string) {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	st := d.Status
	detail := fmt.Sprintf("deployment %s: %d/%d ready, %d updated", d.Name, st.ReadyReplicas, desired, st.UpdatedReplicas)
	switch {
	case st.ObservedGeneration < d.Generation:
		return false, detail + ", spec change not yet observed"
	case st.UpdatedReplicas < desired:
		return false, detail + ", rollout in progress"
	}
	return st.ReadyReplicas >= desired && st.UnavailableReplicas == 0, detail
}

// resolveDeploymentName extracts the Deployment name from a workload string.
// "replicaset/api-server-8446f784fd" → "api-server"
// "deployment/api-server" → "api-server"
// "pod/my-pod" → "my-pod"
func resolveDeploymentName(workload string) string {
	parts := strings.SplitN(workload, "/", 2)
	if len(parts) != 2 {
		return workload
	}
	name := parts[1]
	if parts[0] == "replicaset" {
		// Strip the ReplicaSet hash suffix: "api-server-8446f784fd" → "api-server"
		// RS names are deploy-name + "-" + hash (10 chars)
		if idx := strings.LastIndex(name, "-"); idx > 0 {
			suffix := name[idx+1:]
			if len(suffix) >= 5 && len(suffix) <= 15 {
				return name[:idx]
			}
		}
	}
	return name
}

func (ft *FixTracker) addFixed(rec FixRecord) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.fixed) >= ft.maxLen {
		ft.fixed = ft.fixed[1:]
	}
	ft.fixed = append(ft.fixed, rec)
}

func (ft *FixTracker) addFailed(rec FixRecord) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.failed) >= ft.maxLen {
		ft.failed = ft.failed[1:]
	}
	ft.failed = append(ft.failed, rec)
}

// Fixed returns verified fixes (newest first).
func (ft *FixTracker) Fixed() []FixRecord {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	result := make([]FixRecord, len(ft.fixed))
	for i, r := range ft.fixed {
		result[len(ft.fixed)-1-i] = r
	}
	return result
}

// Failed returns actions that didn't fix the problem (newest first).
func (ft *FixTracker) Failed() []FixRecord {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	result := make([]FixRecord, len(ft.failed))
	for i, r := range ft.failed {
		result[len(ft.failed)-1-i] = r
	}
	return result
}

// Pending returns actions waiting for verification.
func (ft *FixTracker) Pending() []FixRecord {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]FixRecord(nil), ft.pending...)
}

// Stats returns counts.
func (ft *FixTracker) Stats() (pending, fixed, failed int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.pending), len(ft.fixed), len(ft.failed)
}

// clock returns the current time from the injected clock, or time.Now.
func (ft *FixTracker) clock() time.Time {
	if ft.now != nil {
		return ft.now()
	}
	return time.Now()
}

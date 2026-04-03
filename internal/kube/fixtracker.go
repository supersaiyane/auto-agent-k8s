package kube

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
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
}

// FixTracker monitors actions taken and verifies if the workload actually recovered.
type FixTracker struct {
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
		Timestamp: time.Now().UTC(),
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
		// Skip if too old (>15 min) — give up
		if time.Since(rec.Timestamp) > 15*time.Minute {
			rec.Status = "not-fixed"
			rec.VerifiedAt = time.Now().UTC()
			rec.Detail = "timed out — workload did not recover within 15 minutes"
			deps.FixTracker.addFailed(rec)
			klog.V(3).Infof("fixtracker: timed out %s/%s reason=%s", rec.Namespace, rec.Workload, rec.Reason)
			continue
		}

		// Don't check too early — wait at least 30s after action
		if time.Since(rec.Timestamp) < 30*time.Second {
			stillPending = append(stillPending, rec)
			continue
		}

		// Check if the workload is now healthy
		healthy, detail := isWorkloadHealthy(ctx, deps, rec.Namespace, rec.Workload)
		if healthy {
			rec.Status = "fixed"
			rec.VerifiedAt = time.Now().UTC()
			rec.Detail = detail
			deps.FixTracker.addFixed(rec)

			// Record as a verified fix event for the dashboard
			recordEvent(deps, eventsvc.Event{
				Type: eventsvc.Action, Severity: eventsvc.SevInfo,
				Namespace: rec.Namespace, Workload: rec.Workload,
				Reason:  rec.Reason,
				Action:  "verified-fix",
				Message: fmt.Sprintf("Fixed: %s → %s (verified healthy after %s)",
					rec.Reason, rec.Action, time.Since(rec.Timestamp).Round(time.Second)),
			})
			obs.ActionsTotal.WithLabelValues("verified_fix", rec.Namespace, rec.Workload).Inc()
			klog.Infof("fixtracker: VERIFIED FIX %s/%s reason=%s action=%s recovery=%s",
				rec.Namespace, rec.Workload, rec.Reason, rec.Action, time.Since(rec.Timestamp).Round(time.Second))
		} else {
			// Still not healthy — keep checking
			stillPending = append(stillPending, rec)
		}
	}

	deps.FixTracker.mu.Lock()
	deps.FixTracker.pending = stillPending
	deps.FixTracker.mu.Unlock()
}

// isWorkloadHealthy checks if all pods for a workload are Running+Ready.
func isWorkloadHealthy(ctx context.Context, deps *Deps, ns, workload string) (bool, string) {
	// workload format: "replicaset/name-hash" or "deployment/name" or "pod/name"
	pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, ""
	}

	matching := 0
	ready := 0
	for _, p := range pods.Items {
		if ownerName(&p) == workload || "pod/"+p.Name == workload {
			matching++
			allReady := true
			for _, cs := range p.Status.ContainerStatuses {
				if !cs.Ready {
					allReady = false
					break
				}
				// Still in a bad state
				if cs.State.Waiting != nil {
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
		return false, "no matching pods found"
	}
	if ready == matching {
		return true, fmt.Sprintf("%d/%d pods Running+Ready", ready, matching)
	}
	return false, fmt.Sprintf("%d/%d pods ready", ready, matching)
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

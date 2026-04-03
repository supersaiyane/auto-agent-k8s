package kube

import (
	"fmt"
	"sync"
	"time"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/policy"
)

// DryRunLog records what the agent WOULD do in fix mode without actually doing it.
// Active when mode=dry-run. Shows simulated actions in the dashboard.
type DryRunLog struct {
	mu      sync.Mutex
	entries []DryRunEntry
	maxLen  int
}

type DryRunEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Namespace string    `json:"namespace"`
	Workload  string    `json:"workload"`
	Pod       string    `json:"pod,omitempty"`
	Reason    string    `json:"reason"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	Blocked   string    `json:"blocked,omitempty"` // guardrail that would block
}

func NewDryRunLog(maxLen int) *DryRunLog {
	if maxLen <= 0 {
		maxLen = 200
	}
	return &DryRunLog{entries: make([]DryRunEntry, 0, maxLen), maxLen: maxLen}
}

func (d *DryRunLog) Record(e DryRunEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	if len(d.entries) >= d.maxLen {
		d.entries = d.entries[1:]
	}
	d.entries = append(d.entries, e)
}

func (d *DryRunLog) Recent(n int) []DryRunEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	total := len(d.entries)
	if n <= 0 || n > total {
		n = total
	}
	result := make([]DryRunEntry, n)
	for i := 0; i < n; i++ {
		result[i] = d.entries[total-1-i]
	}
	return result
}

// IsDryRun returns true if the policy mode is "dry-run".
func IsDryRun(pol *policy.Policy) bool {
	return pol.Mode == policy.DryRun
}

// SimulateAction records what would happen without executing it.
// Returns the message string describing the simulated action.
func SimulateAction(deps *Deps, ns, wl, pod, reason, actionType, description string) string {
	if deps.DryRunLog == nil {
		return ""
	}

	// Check if guardrails would block
	blocked := ""
	if deps.QuietHours != nil && deps.QuietHours.IsQuiet() {
		blocked = "quiet hours"
	} else if deps.BlastRadius != nil && !deps.BlastRadius.AllowAction(ns) {
		blocked = "blast radius"
	} else {
		crdPol := effectivePolicy(deps, ns, nil)
		if !policyAllowsAction(crdPol) {
			blocked = "requires approval"
		} else if deps.Breaker != nil && deps.Breaker.IsTripped(ns, wl) {
			blocked = "circuit breaker"
		}
	}

	entry := DryRunEntry{
		Namespace: ns,
		Workload:  wl,
		Pod:       pod,
		Reason:    reason,
		Action:    actionType,
		Detail:    description,
		Blocked:   blocked,
	}
	deps.DryRunLog.Record(entry)

	// Record as event for dashboard
	msg := description
	if blocked != "" {
		msg = fmt.Sprintf("[WOULD BE BLOCKED: %s] %s", blocked, description)
	}
	recordEvent(deps, eventsvc.Event{
		Type: eventsvc.Action, Severity: eventsvc.SevInfo,
		Namespace: ns, Workload: wl, Pod: pod,
		Reason:  reason,
		Action:  "dry-run:" + actionType,
		Message: msg,
	})

	if blocked != "" {
		return fmt.Sprintf("_Dry-run_: WOULD %s but blocked by %s.\n", description, blocked)
	}
	return fmt.Sprintf("_Dry-run_: WOULD %s.\n", description)
}

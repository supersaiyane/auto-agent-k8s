package kube

import (
	"fmt"
	"sync"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
)

// effectivePolicy resolves CRD per-policy overrides for a given namespace + labels.
// Returns the first matching policy or nil if none match.
func effectivePolicy(deps *Deps, ns string, labels map[string]string) *crd.Policy {
	if labels == nil {
		return nil
	}
	policies := deps.CRDStore.Match(ns, labels)
	if len(policies) == 0 {
		return nil
	}
	return &policies[0]
}

// effectiveCooldown returns the per-policy cooldown or the global fallback.
func effectiveCooldown(pol *crd.Policy, globalUp, globalDown string) (up, down time.Duration) {
	up = parseDuration(globalUp, "2m")
	down = parseDuration(globalDown, "10m")
	if pol != nil && pol.Cooldown != "" {
		cd := parseDuration(pol.Cooldown, "")
		if cd > 0 {
			up = cd
			down = cd
		}
	}
	return
}

// policyRefusal says why the matching AutoRemediationPolicy refuses an
// action, or "" when it allows it: safety.requireApproval,
// actions.restartStuckPods: false for pod restarts, and
// safety.maxActionsPerHour (ISS-037). The gate, dry-run and the terminal's
// preview all ask this one function.
func policyRefusal(deps *Deps, pol *crd.Policy, actionType string) string {
	switch {
	case pol == nil:
		return ""
	case pol.RequireApproval:
		return "CRD policy requires manual approval"
	case actionType == "delete_pod" && pol.RestartStuckPods != nil && !*pol.RestartStuckPods:
		return fmt.Sprintf("CRD policy %s turns off pod restarts (actions.restartStuckPods: false)", pol.Name)
	case pol.MaxActionsPerHour > 0:
		if deps.PolicyBudget == nil {
			return fmt.Sprintf("CRD policy %s sets maxActionsPerHour but no budget tracker is configured", pol.Name)
		}
		if used := deps.PolicyBudget.Used(policyKey(pol), deps.clock()); used >= pol.MaxActionsPerHour {
			return fmt.Sprintf("CRD policy %s allows %d actions per hour, %d used", pol.Name, pol.MaxActionsPerHour, used)
		}
	}
	return ""
}

func policyKey(pol *crd.Policy) string { return pol.Namespace + "/" + pol.Name }

// recordPolicyAction counts an applied action against its policy's hourly
// budget.
func recordPolicyAction(deps *Deps, m mutation) {
	pol := effectivePolicy(deps, m.Namespace, m.Labels)
	if pol != nil && pol.MaxActionsPerHour > 0 && deps.PolicyBudget != nil {
		deps.PolicyBudget.Record(policyKey(pol), deps.clock())
	}
}

// PolicyBudget counts actions per AutoRemediationPolicy over the last hour,
// for safety.maxActionsPerHour.
type PolicyBudget struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func NewPolicyBudget() *PolicyBudget { return &PolicyBudget{hits: map[string][]time.Time{}} }

// Used returns the actions recorded for key in the hour before now,
// dropping older ones.
func (b *PolicyBudget) Used(key string, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.hits[key][:0]
	for _, t := range b.hits[key] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	b.hits[key] = kept
	return len(kept)
}

// Record adds one action for key at now.
func (b *PolicyBudget) Record(key string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hits[key] = append(b.hits[key], now)
}

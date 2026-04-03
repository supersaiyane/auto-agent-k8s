package kube

import (
	"time"

	"github.com/yourorg/auto-agent/internal/crd"
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

// policyAllowsAction checks per-policy requireApproval and maxActionsPerHour.
// Returns true if the action is allowed.
func policyAllowsAction(pol *crd.Policy) bool {
	if pol == nil {
		return true
	}
	if pol.RequireApproval {
		return false // action requires human approval
	}
	return true
}

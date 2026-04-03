package kube

import (
	"context"
)

// checkGuardrails runs all safety checks before an action.
// Returns (blocked bool, reason string).
func checkGuardrails(ctx context.Context, deps *Deps, ns, wl string, labels map[string]string) (bool, string) {
	// Quiet hours
	if deps.QuietHours != nil && deps.QuietHours.IsQuiet() {
		return true, "quiet hours active — actions suppressed"
	}

	// Blast radius
	if deps.BlastRadius != nil && !deps.BlastRadius.AllowAction(ns) {
		return true, "blast radius limit — too many namespaces affected this hour"
	}

	// CRD per-policy approval
	crdPol := effectivePolicy(deps, ns, labels)
	if !policyAllowsAction(crdPol) {
		return true, "CRD policy requires manual approval"
	}

	// Circuit breaker
	if deps.Breaker != nil && !deps.Breaker.RecordAndCheck(ns, wl) {
		if deps.AlertManager != nil {
			deps.AlertManager.FireCircuitBreaker(ctx, ns, wl)
		}
		return true, "circuit breaker tripped — too many actions on this workload"
	}

	return false, ""
}

// auditAction records an action to the persistent audit log.
func auditAction(deps *Deps, action, ns, wl, pod, reason, result, detail string) {
	if deps.AuditLog != nil {
		deps.AuditLog.RecordAction(action, ns, wl, pod, reason, result, detail, string(deps.Policy.Mode))
	}
}

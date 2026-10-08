package kube

import (
	"context"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// checkGuardrails runs all safety checks before an action.
// Returns (blocked bool, reason string).
func checkGuardrails(ctx context.Context, deps *Deps, m mutation) (bool, string) {
	ns, wl := m.Namespace, m.Workload
	// Quiet hours
	if deps.QuietHours != nil && deps.QuietHours.IsQuiet() {
		return true, "quiet hours active: actions suppressed"
	}

	// Blast radius
	if deps.BlastRadius != nil && !deps.BlastRadius.AllowAction(ns) {
		return true, "blast radius limit: too many namespaces affected this hour"
	}

	// CRD per-policy limits (ISS-037)
	if why := policyRefusal(deps, effectivePolicy(deps, ns, m.Labels), m.ActionType); why != "" {
		return true, why
	}

	// Circuit breaker
	if deps.Breaker != nil && !deps.Breaker.RecordAndCheck(ns, wl) {
		if deps.AlertManager != nil {
			deps.AlertManager.FireCircuitBreaker(ctx, ns, wl)
		}
		return true, "circuit breaker tripped: too many actions on this workload"
	}

	return false, ""
}

// auditAction records an action to the persistent audit log.
func auditAction(deps *Deps, action, ns, wl, pod, reason, result, detail string) {
	if deps.AuditLog != nil {
		deps.AuditLog.RecordAction(action, ns, wl, pod, reason, result, detail, string(deps.Policy().Mode))
	}
	// The same decision as an event, so it reaches the controller and the
	// dashboard from node agents too (ISS-061).
	sev := eventsvc.SevInfo
	switch result {
	case "blocked":
		sev = eventsvc.SevWarning
	case "failed":
		sev = eventsvc.SevCritical
	}
	recordEvent(deps, eventsvc.Event{Type: eventsvc.Audit, Severity: sev, Namespace: ns, Workload: wl, Pod: pod,
		Node: deps.NodeName, Reason: reason, Action: action, Result: result, Message: detail})
}

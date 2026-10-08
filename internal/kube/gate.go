package kube

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/escalation"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// gateOutcome says what the mutation gate did with a requested change.
type gateOutcome int

const (
	gateSkipped   gateOutcome = iota // observe mode: nothing done, nothing to report
	gateSuggested                    // suggest mode: the change is described only
	gateSimulated                    // dry-run mode: the change is recorded in the dry-run log
	gateBlocked                      // a guardrail or the rate limiter refused the change
	gateFailed                       // the change was sent and the API server returned an error
	gateApplied                      // the change was sent and succeeded
)

// mutation is one change to the cluster requested by a handler or a loop.
type mutation struct {
	Namespace  string
	Workload   string
	Pod        string
	Labels     map[string]string
	Reason     string // what was detected, e.g. "CrashLoopBackOff"
	ActionType string // metric and audit label, e.g. "delete_pod"
	SuccessMsg string // shown when applied, e.g. "deleted pod"
	SuggestMsg string // shown in suggest mode, e.g. "delete the pod"
	Apply      func() error
	ApprovedBy string // who approved an R3 change (phase 15); empty when none was needed
}

// approvalNote adds who approved the change to an audit detail.
func approvalNote(m mutation, detail string) string {
	if m.ApprovedBy == "" {
		return detail
	}
	if detail == "" {
		return "approved by " + m.ApprovedBy
	}
	return detail + "; approved by " + m.ApprovedBy
}

// applyMutation is the only path by which the agent changes the cluster.
// It checks the fix scope, the mode, then the guardrails, then the rate
// limiter, and only then calls m.Apply. TestMutationsOnlyThroughGate fails if any mutating
// client call in this package is made outside a closure handed to this gate.
func applyMutation(ctx context.Context, deps *Deps, m mutation) (gateOutcome, string) {
	pol := deps.Policy()
	// Outside the fix scope (ADR-002) the change is only described, in every
	// mode but observe, even where RBAC would allow it. Cluster-scoped
	// changes (nodes) have no namespace and are governed by mode and guardrails.
	if m.Namespace != "" && !pol.Fixable(m.Namespace) && pol.Mode != policy.Observe {
		why := fmt.Sprintf("%s is outside the fix scope", m.Namespace)
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "suggested", why+": "+m.SuggestMsg)
		return gateSuggested, fmt.Sprintf("_Suggest_ (%s): %s.\n", why, m.SuggestMsg)
	}
	switch pol.Mode {
	case policy.Fix:
	case policy.Suggest:
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "suggested", m.SuggestMsg)
		return gateSuggested, fmt.Sprintf("_Suggest_: %s.\n", m.SuggestMsg)
	case policy.DryRun:
		msg := SimulateAction(deps, m.Namespace, m.Workload, m.Pod, m.Reason, m.ActionType, m.SuccessMsg)
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "simulated", m.SuccessMsg)
		return gateSimulated, msg
	default:
		return gateSkipped, ""
	}

	if blocked, why := checkGuardrails(ctx, deps, m.Namespace, m.Workload, m.Labels); blocked {
		klog.Infof("gate: BLOCKED %s %s/%s reason=%s by=%s", m.ActionType, m.Namespace, m.Workload, m.Reason, why)
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "blocked", approvalNote(m, why))
		return gateBlocked, fmt.Sprintf("_Blocked_: %s.\n", why)
	}
	// A missing limiter fails closed: no limiter means no budget, not no limit.
	if deps.Limiter == nil || !deps.Limiter.Allow() {
		klog.Infof("gate: RATE LIMITED %s %s/%s reason=%s", m.ActionType, m.Namespace, m.Workload, m.Reason)
		obs.RateLimitedTotal.Inc()
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "blocked", approvalNote(m, "rate limited"))
		return gateBlocked, "_Action_: rate limited, skipping.\n"
	}

	if err := m.Apply(); err != nil {
		klog.Warningf("gate: %s failed for %s/%s: %v", m.ActionType, m.Namespace, m.Workload, err)
		obs.HandlerErrorsTotal.WithLabelValues(m.Reason, m.ActionType).Inc()
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "failed", approvalNote(m, err.Error()))
		escalateFailedFix(deps, m, err)
		return gateFailed, fmt.Sprintf("_Action_: %s failed: %v\n", m.ActionType, err)
	}

	klog.Infof("gate: APPLIED %s %s/%s reason=%s", m.ActionType, m.Namespace, m.Workload, m.Reason)
	obs.ActionsTotal.WithLabelValues(m.ActionType, m.Namespace, m.Workload).Inc()
	auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "success", approvalNote(m, ""))
	return gateApplied, fmt.Sprintf("_Action_: %s.\n", m.SuccessMsg)
}

// mergePatch encodes v as a JSON merge patch. Field-level writes use merge
// patches so they never overwrite fields they did not mean to change
// (CLAUDE.md constraint 6). A nil map value deletes the key.
func mergePatch(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only maps of strings, numbers, bools and nils reach here.
		panic(fmt.Sprintf("mergePatch: %v", err))
	}
	return b
}

// escalate pages the configured channels (PagerDuty, OpsGenie, email) for
// a critical finding, after Slack and in the background (ISS-080). Callers
// have already deduplicated it.
func escalate(deps *Deps, reason, ns, wl, body string) {
	if !deps.Escalation.Configured() {
		return
	}
	title := reason + " " + wl
	if ns != "" {
		title = reason + " " + ns + "/" + wl
	}
	deps.Escalation.Send(escalation.Incident{Title: title, Body: body, Severity: escalation.SevCritical,
		Namespace: ns, Workload: wl, Source: "auto-agent"})
}

// escalateFailedFix pages once per workload and action per dedup window
// when a fix the gate sent is refused by the API server.
func escalateFailedFix(deps *Deps, m mutation, err error) {
	if !deps.Escalation.Configured() || deps.Dedup == nil || !deps.Dedup.Check(dedupKey(m.Namespace, m.Workload, "EscalateFailedFix/"+m.ActionType)) {
		return
	}
	escalate(deps, "FixFailed", m.Namespace, m.Workload, fmt.Sprintf("%s failed for %s (%s): %v", m.ActionType, m.Workload, m.Reason, err))
}

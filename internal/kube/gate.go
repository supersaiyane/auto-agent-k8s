package kube

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/klog/v2"

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
}

// applyMutation is the only path by which the agent changes the cluster.
// It checks the mode, then the guardrails, then the rate limiter, and only
// then calls m.Apply. TestMutationsOnlyThroughGate fails if any mutating
// client call in this package is made outside a closure handed to this gate.
func applyMutation(ctx context.Context, deps *Deps, m mutation) (gateOutcome, string) {
	switch deps.Policy().Mode {
	case policy.Fix:
	case policy.Suggest:
		return gateSuggested, fmt.Sprintf("_Suggest_: %s.\n", m.SuggestMsg)
	case policy.DryRun:
		return gateSimulated, SimulateAction(deps, m.Namespace, m.Workload, m.Pod, m.Reason, m.ActionType, m.SuccessMsg)
	default:
		return gateSkipped, ""
	}

	if blocked, why := checkGuardrails(ctx, deps, m.Namespace, m.Workload, m.Labels); blocked {
		klog.Infof("gate: BLOCKED %s %s/%s reason=%s by=%s", m.ActionType, m.Namespace, m.Workload, m.Reason, why)
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "blocked", why)
		return gateBlocked, fmt.Sprintf("_Blocked_: %s.\n", why)
	}
	// A missing limiter fails closed: no limiter means no budget, not no limit.
	if deps.Limiter == nil || !deps.Limiter.Allow() {
		klog.Infof("gate: RATE LIMITED %s %s/%s reason=%s", m.ActionType, m.Namespace, m.Workload, m.Reason)
		obs.RateLimitedTotal.Inc()
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "blocked", "rate limited")
		return gateBlocked, "_Action_: rate limited, skipping.\n"
	}

	if err := m.Apply(); err != nil {
		klog.Warningf("gate: %s failed for %s/%s: %v", m.ActionType, m.Namespace, m.Workload, err)
		obs.HandlerErrorsTotal.WithLabelValues(m.Reason, m.ActionType).Inc()
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "failed", err.Error())
		return gateFailed, fmt.Sprintf("_Action_: %s failed: %v\n", m.ActionType, err)
	}

	klog.Infof("gate: APPLIED %s %s/%s reason=%s", m.ActionType, m.Namespace, m.Workload, m.Reason)
	obs.ActionsTotal.WithLabelValues(m.ActionType, m.Namespace, m.Workload).Inc()
	auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "success", "")
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

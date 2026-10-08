package kube

import (
	"fmt"
	"strings"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// GateCheck is one guardrail's answer to "would a fix in this namespace be
// applied now?" (the terminal's `agent gate`, PLAN-003 3.3).
type GateCheck struct {
	Check  string
	Pass   bool
	Detail string
}

// GatePreview answers check by check, without acting and without spending
// anything: it uses only the read-only side of each guardrail (no
// Limiter.Allow, no BlastRadius.AllowAction, no Breaker.RecordAndCheck).
func GatePreview(deps *Deps, ns string) []GateCheck {
	pol := deps.Policy()
	out := []GateCheck{{Check: "mode", Pass: pol.Mode == policy.Fix, Detail: modeDetail(pol.Mode)}}
	if pol.Fixable(ns) {
		out = append(out, GateCheck{"fix scope", true, ns + " is in the fix scope"})
	} else {
		out = append(out, GateCheck{"fix scope", false, ns + " is outside the fix scope: fixes are only suggested"})
	}
	quiet := deps.QuietHours != nil && deps.QuietHours.IsQuiet()
	out = append(out, GateCheck{"quiet hours", !quiet, map[bool]string{true: "quiet hours are active", false: "not in quiet hours"}[quiet]})
	if deps.BlastRadius != nil {
		ok := deps.BlastRadius.WouldAllow(ns)
		out = append(out, GateCheck{"blast radius", ok, fmt.Sprintf("%d namespaces acted on this hour", deps.BlastRadius.AffectedNamespaces())})
	}
	if deps.Limiter == nil {
		out = append(out, GateCheck{"rate limit", false, "no rate limiter: the gate fails closed"})
	} else {
		left := deps.Limiter.Remaining()
		out = append(out, GateCheck{"rate limit", left > 0, fmt.Sprintf("%d actions left in the window", left)})
	}
	if deps.Breaker != nil {
		var tripped []string
		for _, w := range deps.Breaker.TrippedWorkloads() {
			if strings.HasPrefix(w, ns+"/") {
				tripped = append(tripped, strings.TrimPrefix(w, ns+"/"))
			}
		}
		detail := "no workload tripped"
		if len(tripped) > 0 {
			detail = "tripped, so blocked: " + strings.Join(tripped, ", ") + "; other workloads pass"
		}
		out = append(out, GateCheck{"circuit breaker", true, detail})
	}
	out = append(out, policyChecks(deps, ns))
	return out
}

// policyChecks names the AutoRemediationPolicies in ns that limit actions.
// The preview has no pod labels, so it lists them rather than guessing which
// workload each one selects (ISS-081).
func policyChecks(deps *Deps, ns string) GateCheck {
	var limits []string
	approval := false
	for _, p := range deps.CRDStore.List(ns) {
		var what []string
		if p.RequireApproval {
			what, approval = append(what, "requires approval"), true
		}
		if p.RestartStuckPods != nil && !*p.RestartStuckPods {
			what = append(what, "no pod restarts")
		}
		if p.MaxActionsPerHour > 0 {
			what = append(what, fmt.Sprintf("%d actions per hour", p.MaxActionsPerHour))
		}
		if len(what) > 0 {
			limits = append(limits, p.Name+" ("+strings.Join(what, ", ")+")")
		}
	}
	if len(limits) == 0 {
		return GateCheck{"policy limits", true, "no AutoRemediationPolicy here limits actions"}
	}
	return GateCheck{"policy limits", !approval, "for the workloads they select: " + strings.Join(limits, "; ")}
}

func modeDetail(m policy.Mode) string {
	switch m {
	case policy.Fix:
		return "fix: changes are applied"
	case policy.DryRun:
		return "dry-run: changes are simulated and recorded"
	case policy.Suggest:
		return "suggest: changes are described only"
	}
	return string(m) + ": nothing is done"
}

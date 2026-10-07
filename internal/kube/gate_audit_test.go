package kube

import (
	"context"
	"errors"
	"testing"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// ISS-061: every gate decision becomes an audit event, so node agents'
// actions reach the controller's log and the dashboard.
func TestGateDecisionsAreAuditEvents(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   policy.Mode
		open   bool
		apply  error
		result string
		sev    eventsvc.Severity
	}{
		{"suggest", policy.Suggest, true, nil, "suggested", eventsvc.SevInfo},
		{"dry-run", policy.DryRun, true, nil, "simulated", eventsvc.SevInfo},
		{"applied", policy.Fix, true, nil, "success", eventsvc.SevInfo},
		{"failed", policy.Fix, true, errors.New("conflict"), "failed", eventsvc.SevCritical},
		{"blocked without a rate budget", policy.Fix, false, nil, "blocked", eventsvc.SevWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFindingHarness(t)
			p := testPolicy()
			p.Mode = tc.mode
			h.deps.Policies = policy.Static(p)
			h.deps.NodeName = "node-1"
			if tc.open {
				openGuardrails(h.deps)
			}
			applyMutation(context.Background(), h.deps, mutation{Namespace: "default", Workload: "api", Pod: "api-1",
				Reason: "CrashLoopBackOff", ActionType: "delete_pod", SuccessMsg: "deleted pod", SuggestMsg: "delete the pod",
				Apply: func() error { return tc.apply }})
			var audits []eventsvc.Event
			for _, e := range h.rec.Recent(10) {
				if e.Type == eventsvc.Audit {
					audits = append(audits, e)
				}
			}
			if len(audits) != 1 {
				t.Fatalf("want one audit event, got %+v", audits)
			}
			a := audits[0]
			if a.Result != tc.result || a.Severity != tc.sev || a.Action != "delete_pod" || a.Node != "node-1" || a.Workload != "api" {
				t.Fatalf("audit event: %+v", a)
			}
		})
	}
}

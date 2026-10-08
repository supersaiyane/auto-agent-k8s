package kube

import (
	"context"
	"fmt"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// CheckAnomalies evaluates the PromQL anomaly rules in AutoRemediationPolicy
// objects: a rule fires when its query returns more than its threshold
// (zScoreThreshold, default 1). The phase 12 audit found it ran policies in
// namespaces outside the watch scope, dropped failed queries, and posted to
// Slack only, with no event or rung. Rung R1: anomalies need a person to
// interpret them.
// Must be called only by the leader.
func CheckAnomalies(ctx context.Context, deps *Deps) {
	for _, ns := range deps.CRDStore.AllNamespaces() {
		if !deps.Policy().Watched(ns) {
			continue
		}
		for _, pol := range deps.CRDStore.List(ns) {
			for _, rule := range pol.Anomalies {
				if rule.PromQL == "" {
					continue
				}
				val, err := deps.Metrics.QueryInstant(ctx, rule.PromQL)
				if err != nil {
					obs.HandlerErrorsTotal.WithLabelValues("anomaly", "query").Inc()
					continue
				}
				threshold := rule.ZScoreThreshold
				if threshold <= 0 {
					threshold = 1.0
				}
				if val <= threshold {
					continue
				}
				f := finding{Reason: "Anomaly", Namespace: ns, Workload: "policy/" + pol.Name, Subject: rule.Name,
					Severity: eventsvc.SevWarning, Rung: RungGuided,
					Summary: fmt.Sprintf("rule `%s` fired: %.4f is above %.4f", rule.Name, val, threshold),
					Details: []string{fmt.Sprintf("PromQL: `%s`", rule.PromQL)},
					Fix:     "compare with the workload's recent deploys and baselines on the dashboard"}
				if pol.RunbookURL != "" {
					f.Fix = "follow the runbook: " + pol.RunbookURL
				}
				if report(ctx, deps, f) {
					obs.AnomaliesDetectedTotal.WithLabelValues(pol.Name, ns, rule.Name).Inc()
				}
			}
		}
	}
}

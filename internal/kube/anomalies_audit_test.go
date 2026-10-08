package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// Phase 12 audit of CheckAnomalies.
//
// Claims: PromQL anomaly rules from AutoRemediationPolicy objects.
// Condition: the query's value is above zScoreThreshold (default 1).
// Bugs found: (1) policies in namespaces outside the watch scope were run
// and reported; (2) a failed query was dropped; (3) it posted to Slack only,
// with no event, so the dashboard never showed it, and no rung. Rung R1.
func TestAudit_Anomalies(t *testing.T) {
	h := newFindingHarness(t)
	h.deps.Metrics = &mockMetrics{gate: 5}
	store := crd.NewStore()
	store.Update("default", []crd.Policy{{Name: "api", RunbookURL: "https://runbooks.example.test/api", Anomalies: []crd.AnomalyRule{
		{Name: "errors", PromQL: "rate(errors[5m])", ZScoreThreshold: 2},
		{Name: "quiet", PromQL: "rate(ok[5m])", ZScoreThreshold: 10},
		{Name: "empty"},
		{Name: "default-threshold", PromQL: "x"},
	}}})
	store.Update("payments", []crd.Policy{{Name: "hidden", Anomalies: []crd.AnomalyRule{{Name: "r", PromQL: "y"}}}})
	h.deps.CRDStore = store
	CheckAnomalies(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "Anomaly", RungGuided, 2), "\n")
	for _, want := range []string{"policy/api", "rule `errors` fired: 5.0000 is above 2.0000", "rule `default-threshold` fired", "runbooks.example.test/api"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if strings.Contains(msgs, "hidden") {
		t.Error("a policy outside the watch scope is not run")
	}

	failing := newFindingHarness(t)
	failing.deps.Metrics = &mockMetrics{gateErr: errors.New("prometheus down")}
	failing.deps.CRDStore = store
	before := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("anomaly", "query"))
	CheckAnomalies(context.Background(), failing.deps)
	if testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("anomaly", "query")) <= before {
		t.Fatal("a failed query is counted")
	}
	failing.quiet(t)
}

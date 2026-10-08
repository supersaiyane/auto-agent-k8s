package kube

import (
	"strings"
	"testing"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
)

func gateTable(rows []GateCheck) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Check+"="+map[bool]string{true: "pass", false: "block"}[r.Pass])
	}
	return strings.Join(out, " ")
}

// PLAN-003 3.3: the preview names each check and spends nothing.
func TestGatePreview(t *testing.T) {
	deps, _ := newHandlerTestDeps(t)
	openGuardrails(deps)
	deps.Policy().Mode = policy.Fix
	before := deps.Limiter.Remaining()
	if got := gateTable(GatePreview(deps, "default")); got != "mode=pass fix scope=pass quiet hours=pass blast radius=pass rate limit=pass circuit breaker=pass policy approval=pass" {
		t.Fatalf("open gate: %s", got)
	}
	for i := 0; i < 3; i++ {
		GatePreview(deps, "default")
	}
	if deps.Limiter.Remaining() != before || deps.BlastRadius.AffectedNamespaces() != 0 {
		t.Fatal("a preview spends nothing")
	}

	deps.Policy().Mode = policy.DryRun
	deps.QuietHours = NewQuietHours("00:00-00:01,00:01-00:00")
	deps.Limiter = ratelimit.NewActionLimiter(0, time.Hour)
	deps.Breaker = ratelimit.NewCircuitBreaker(1, time.Hour)
	deps.Breaker.RecordAndCheck("default", "api")
	deps.Breaker.RecordAndCheck("default", "api")
	rows := GatePreview(deps, "payments")
	if got := gateTable(rows); got != "mode=block fix scope=block quiet hours=block blast radius=pass rate limit=block circuit breaker=pass policy approval=pass" {
		t.Fatalf("closed gate: %s", got)
	}
	if d := GatePreview(deps, "default")[5].Detail; !strings.Contains(d, "tripped, so blocked: api") {
		t.Fatalf("breaker detail: %s", d)
	}
	deps.Limiter = nil
	if r := GatePreview(deps, "default")[4]; r.Pass || !strings.Contains(r.Detail, "fails closed") {
		t.Fatalf("no limiter: %+v", r)
	}
	for _, m := range []policy.Mode{policy.Suggest, policy.Observe} {
		if modeDetail(m) == "" {
			t.Fatal("mode detail")
		}
	}
}

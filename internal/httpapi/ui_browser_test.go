//go:build browser

package httpapi

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// PLAN-002 11.2, ISS-060: the real dashboard in headless Chrome, over a fake
// cluster and an event log seeded with hostile values. Run by make ui-test.
func TestDashboardInBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the browser test")
	}
	rec := events.NewRecorder(100)
	now := time.Now().UTC()
	for _, e := range []events.Event{
		{Type: events.Incident, Severity: events.SevCritical, Namespace: "default", Workload: "replicaset/api-1", Reason: "CrashLoopBackOff", Message: "exit 1", Rung: "R1", Timestamp: now},
		{Type: events.Audit, Severity: events.SevInfo, Namespace: "default", Workload: "replicaset/api-1", Reason: "CrashLoopBackOff", Action: "delete_pod", Result: "simulated", Node: "node-a", Timestamp: now},
		{Type: events.Audit, Severity: events.SevWarning, Namespace: "default", Workload: "replicaset/api-1", Reason: "OOMKilled", Action: "delete_pod", Result: "blocked", Message: "quiet hours", Timestamp: now},
		{Type: events.Action, Severity: events.SevInfo, Namespace: "default", Workload: "replicaset/api-1", Reason: "CrashLoopBackOff", Action: "verified-fix", Timestamp: now},
		{Type: events.Incident, Severity: events.SevWarning, Namespace: "payments", Workload: "deployment/ledger", Reason: "PaymentsOnlyReason", Timestamp: now},
		// Hostile values: markup, a quote that would break out of an attribute or handler, a javascript: link.
		{Type: events.Incident, Severity: events.SevWarning, Namespace: "default", Reason: `<img src=x onerror="window.__pwned=1">`,
			Message: `'); window.__pwned=1; //`, Workload: `x' onclick='window.__pwned=1`, LogURL: "javascript:window.__pwned=1", Timestamp: now},
	} {
		rec.Record(e)
	}
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orders"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1-abc", Namespace: "default"}, Spec: corev1.PodSpec{NodeName: "node-a",
			Containers: []corev1.Container{{Name: "app", Image: "app:v1"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}},
	)
	// The Settings tab against a policy that applies saves at once, as the
	// hot reloader does within seconds (ADR-002).
	var mu sync.Mutex
	pol := policy.Load(func(k string) string {
		return map[string]string{"FIX_NAMESPACES": "default", "FIX_CEILING": "default,payments"}[k]
	})
	current := func() *policy.Policy { mu.Lock(); defer mu.Unlock(); return pol }
	s := NewServer(":0", rec, &AgentMeta{Version: "ui-test", Mode: "dry-run", NodeName: "node-a"}, kc, Options{
		DashboardToken: "ui-token", AllowNamespace: func(ns string) bool { return current().Watched(ns) },
		Scope: ScopeOptions{Policy: current, Save: func(_ context.Context, names []string, _ string) error {
			mu.Lock()
			defer mu.Unlock()
			for _, n := range names {
				if !pol.InCeiling(n) {
					return fmt.Errorf("%s: %w", n, kube.ErrOutsideCeiling)
				}
			}
			pol = pol.WithFixOverride(names)
			return nil
		}},
	})
	srv := httptest.NewServer(s.srv.Handler)
	defer srv.Close()

	out, err := exec.Command(node, "../../scripts/ui-test.mjs", srv.URL, "ui-token").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("browser test failed: %v", err)
	}
}

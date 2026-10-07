package kube

import (
	"testing"
	"time"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// ISS-061: the report comes from the event log, forwarded events included.
func TestComplianceFromEvents(t *testing.T) {
	at := func(m int) time.Time { return testNow.Add(time.Duration(m) * time.Minute) }
	evts := []eventsvc.Event{
		{Type: eventsvc.Incident, Reason: "CrashLoopBackOff", Namespace: "a", Workload: "api", Timestamp: at(-60)},
		{Type: eventsvc.Action, Action: "verified-fix", Namespace: "a", Workload: "api", Timestamp: at(-50)},
		{Type: eventsvc.Incident, Reason: "OOMKilled", Namespace: "b", Workload: "db", Timestamp: at(-40)},
		{Type: eventsvc.Action, Action: "verified-fix", Namespace: "b", Workload: "db", Timestamp: at(-45)}, // before the incident
		{Type: eventsvc.Incident, Reason: "Old", Namespace: "a", Workload: "x", Timestamp: at(-60 * 24 * 40)},
		{Type: eventsvc.Audit, Action: "delete_pod", Result: "success", Timestamp: at(-55)},
		{Type: eventsvc.Audit, Action: "delete_pod", Result: "blocked", Timestamp: at(-54)},
		{Type: eventsvc.Audit, Action: "evict_pod", Result: "simulated", Timestamp: at(-53)},
	}
	r := ComplianceFromEvents(evts, testNow.AddDate(0, 0, -30), testNow, testNow.Add(-2*time.Hour))
	if r.TotalIncidents != 2 || r.AutoRemediated != 1 || r.ManualRequired != 1 || r.Blocked != 1 {
		t.Fatalf("counts: %+v", r)
	}
	if r.AvgMTTRSeconds != 600 || r.RemediationRate != 50 || r.UptimeSeconds != 7200 {
		t.Fatalf("mttr %v rate %v uptime %v", r.AvgMTTRSeconds, r.RemediationRate, r.UptimeSeconds)
	}
	if r.ActionsByType["delete_pod"] != 1 || r.ActionsByType["evict_pod"] != 0 || r.IncidentsByNs["b"] != 1 {
		t.Fatalf("breakdown: %+v", r)
	}
	if empty := ComplianceFromEvents(nil, testNow, testNow, testNow); empty.TotalIncidents != 0 || empty.RemediationRate != 0 {
		t.Fatal("no events, empty report")
	}
}

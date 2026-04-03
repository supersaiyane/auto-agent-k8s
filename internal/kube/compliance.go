package kube

import (
	"fmt"
	"sync"
	"time"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
)

// ComplianceTracker collects data for compliance/SLA reporting.
type ComplianceTracker struct {
	mu        sync.Mutex
	incidents []incidentRecord
	actions   []actionRecord
	startTime time.Time
}

type incidentRecord struct {
	Timestamp  time.Time
	Reason     string
	Namespace  string
	Workload   string
	DetectedAt time.Time
	FixedAt    time.Time // zero if not fixed
}

type actionRecord struct {
	Timestamp time.Time
	Action    string
	Namespace string
	Workload  string
	Result    string // success, failed, blocked
	Duration  time.Duration
}

// ComplianceReport is the monthly compliance summary.
type ComplianceReport struct {
	Period            string  `json:"period"`
	TotalIncidents    int     `json:"totalIncidents"`
	AutoRemediated    int     `json:"autoRemediated"`
	ManualRequired    int     `json:"manualRequired"`
	Blocked           int     `json:"blocked"`
	AvgMTTRSeconds    float64 `json:"avgMttrSeconds"`
	IncidentsByReason map[string]int `json:"incidentsByReason"`
	IncidentsByNs     map[string]int `json:"incidentsByNamespace"`
	ActionsByType     map[string]int `json:"actionsByType"`
	RemediationRate   float64 `json:"remediationRate"` // percentage
	UptimeSeconds     float64 `json:"uptimeSeconds"`
}

func NewComplianceTracker() *ComplianceTracker {
	return &ComplianceTracker{
		incidents: make([]incidentRecord, 0),
		actions:   make([]actionRecord, 0),
		startTime: time.Now(),
	}
}

func (ct *ComplianceTracker) RecordIncident(reason, ns, workload string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.incidents = append(ct.incidents, incidentRecord{
		Timestamp: time.Now(), Reason: reason, Namespace: ns,
		Workload: workload, DetectedAt: time.Now(),
	})
}

func (ct *ComplianceTracker) RecordAction(action, ns, workload, result string, duration time.Duration) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.actions = append(ct.actions, actionRecord{
		Timestamp: time.Now(), Action: action, Namespace: ns,
		Workload: workload, Result: result, Duration: duration,
	})
	// Mark the most recent matching incident as fixed
	for i := len(ct.incidents) - 1; i >= 0; i-- {
		if ct.incidents[i].Namespace == ns && ct.incidents[i].Workload == workload && ct.incidents[i].FixedAt.IsZero() {
			ct.incidents[i].FixedAt = time.Now()
			break
		}
	}
}

// GenerateReport creates a compliance report for the given time window.
func (ct *ComplianceTracker) GenerateReport(since time.Time) ComplianceReport {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	report := ComplianceReport{
		Period:            fmt.Sprintf("%s to %s", since.Format("2006-01-02"), time.Now().Format("2006-01-02")),
		IncidentsByReason: make(map[string]int),
		IncidentsByNs:     make(map[string]int),
		ActionsByType:     make(map[string]int),
		UptimeSeconds:     time.Since(ct.startTime).Seconds(),
	}

	var totalMTTR time.Duration
	var mttrCount int

	for _, inc := range ct.incidents {
		if inc.Timestamp.Before(since) {
			continue
		}
		report.TotalIncidents++
		report.IncidentsByReason[inc.Reason]++
		report.IncidentsByNs[inc.Namespace]++
		if !inc.FixedAt.IsZero() {
			report.AutoRemediated++
			mttr := inc.FixedAt.Sub(inc.DetectedAt)
			totalMTTR += mttr
			mttrCount++
		} else {
			report.ManualRequired++
		}
	}

	for _, act := range ct.actions {
		if act.Timestamp.Before(since) {
			continue
		}
		report.ActionsByType[act.Action]++
		if act.Result == "blocked" {
			report.Blocked++
		}
	}

	if mttrCount > 0 {
		report.AvgMTTRSeconds = totalMTTR.Seconds() / float64(mttrCount)
	}
	if report.TotalIncidents > 0 {
		report.RemediationRate = float64(report.AutoRemediated) / float64(report.TotalIncidents) * 100
	}

	return report
}

// FromEvents builds compliance data from the event recorder.
func (ct *ComplianceTracker) FromEvents(recorder *eventsvc.Recorder) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	events := recorder.Recent(0) // all
	for _, e := range events {
		switch e.Type {
		case eventsvc.Incident:
			ct.incidents = append(ct.incidents, incidentRecord{
				Timestamp: e.Timestamp, Reason: e.Reason,
				Namespace: e.Namespace, Workload: e.Workload,
				DetectedAt: e.Timestamp,
			})
		case eventsvc.Action:
			ct.actions = append(ct.actions, actionRecord{
				Timestamp: e.Timestamp, Action: e.Action,
				Namespace: e.Namespace, Workload: e.Workload,
				Result: "success",
			})
		}
	}
}

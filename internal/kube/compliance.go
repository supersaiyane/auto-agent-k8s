package kube

import (
	"fmt"
	"time"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// ComplianceReport summarises incidents and remediation over a period.
type ComplianceReport struct {
	Period            string         `json:"period"`
	TotalIncidents    int            `json:"totalIncidents"`
	AutoRemediated    int            `json:"autoRemediated"`
	ManualRequired    int            `json:"manualRequired"`
	Blocked           int            `json:"blocked"`
	AvgMTTRSeconds    float64        `json:"avgMttrSeconds"`
	IncidentsByReason map[string]int `json:"incidentsByReason"`
	IncidentsByNs     map[string]int `json:"incidentsByNamespace"`
	ActionsByType     map[string]int `json:"actionsByType"`
	RemediationRate   float64        `json:"remediationRate"` // percentage
	UptimeSeconds     float64        `json:"uptimeSeconds"`
}

// ComplianceFromEvents computes the report from the controller's event log
// (ISS-061). It is stateless, so it counts node agents' forwarded events and
// gives the same answer on every request. An incident counts as remediated
// when a verified fix for the same workload follows it; time to recover is
// measured to the first such fix. Actions are the gate's audit events.
func ComplianceFromEvents(evts []eventsvc.Event, since, now, started time.Time) ComplianceReport {
	r := ComplianceReport{
		Period:            fmt.Sprintf("%s to %s", since.Format("2006-01-02"), now.Format("2006-01-02")),
		IncidentsByReason: map[string]int{},
		IncidentsByNs:     map[string]int{},
		ActionsByType:     map[string]int{},
		UptimeSeconds:     now.Sub(started).Seconds(),
	}
	fixes := map[string][]time.Time{} // namespace/workload -> verified fix times
	for _, e := range evts {
		if e.Type == eventsvc.Action && e.Action == "verified-fix" {
			k := e.Namespace + "/" + e.Workload
			fixes[k] = append(fixes[k], e.Timestamp)
		}
	}
	var mttr time.Duration
	for _, e := range evts {
		if e.Timestamp.Before(since) {
			continue
		}
		switch e.Type {
		case eventsvc.Incident:
			r.TotalIncidents++
			r.IncidentsByReason[e.Reason]++
			r.IncidentsByNs[e.Namespace]++
			if fixed, ok := firstAfter(fixes[e.Namespace+"/"+e.Workload], e.Timestamp); ok {
				r.AutoRemediated++
				mttr += fixed.Sub(e.Timestamp)
			}
		case eventsvc.Audit:
			switch e.Result {
			case "success":
				r.ActionsByType[e.Action]++
			case "blocked":
				r.Blocked++
			}
		}
	}
	r.ManualRequired = r.TotalIncidents - r.AutoRemediated
	if r.AutoRemediated > 0 {
		r.AvgMTTRSeconds = mttr.Seconds() / float64(r.AutoRemediated)
	}
	if r.TotalIncidents > 0 {
		r.RemediationRate = float64(r.AutoRemediated) / float64(r.TotalIncidents) * 100
	}
	return r
}

// firstAfter returns the earliest time in ts at or after t.
func firstAfter(ts []time.Time, t time.Time) (time.Time, bool) {
	var best time.Time
	found := false
	for _, x := range ts {
		if !x.Before(t) && (!found || x.Before(best)) {
			best, found = x, true
		}
	}
	return best, found
}

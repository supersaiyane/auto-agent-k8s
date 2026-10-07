package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
)

// ExtendedDeps are the optional trackers behind the extended endpoints. They
// are passed in Options at construction (PLAN-002 9.4); any may be nil.
type ExtendedDeps struct {
	Compliance *kube.ComplianceTracker
	Learning   *kube.LearningMode
	Deploys    *kube.DeployTracker
	DryRun     *kube.DryRunLog
	Fixes      *kube.FixTracker
}

func (s *Server) handleCompliance(w http.ResponseWriter, r *http.Request) {
	if s.ext.Compliance == nil {
		writeJSON(w, map[string]string{"error": "not initialized"})
		return
	}
	since := time.Now().Add(-30 * 24 * time.Hour)
	if v := r.URL.Query().Get("days"); v != "" {
		var days int
		if _, err := fmt.Sscanf(v, "%d", &days); err == nil && days > 0 {
			since = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		}
	}
	writeJSON(w, s.ext.Compliance.GenerateReport(since))
}

func (s *Server) handleBaselines(w http.ResponseWriter, r *http.Request) {
	if s.ext.Learning == nil {
		writeJSON(w, map[string]interface{}{"learning": false, "baselines": map[string]interface{}{}})
		return
	}
	writeJSON(w, map[string]interface{}{
		"learning":  s.ext.Learning.IsLearning(),
		"baselines": s.ext.Learning.AllBaselines(),
	})
}

func (s *Server) handleDeploys(w http.ResponseWriter, r *http.Request) {
	if s.ext.Deploys == nil {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, s.ext.Deploys.All())
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	if s.ext.DryRun == nil {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, s.ext.DryRun.Recent(100))
}

// handleFixes returns verified fixes, pending verifications, and failed fixes.
func (s *Server) handleFixes(w http.ResponseWriter, r *http.Request) {
	// Pull verified fixes from the event recorder (file-backed, survives restarts)
	allEvents := s.recorder.Recent(0)
	var fixedList, pendingList, failedList []map[string]interface{}

	for _, e := range allEvents {
		if e.Action == "verified-fix" {
			fixedList = append(fixedList, map[string]interface{}{
				"timestamp": e.Timestamp, "namespace": e.Namespace, "workload": e.Workload,
				"reason": e.Reason, "action": "fix", "status": "fixed",
				"detail": e.Message, "verifiedAt": e.Timestamp,
			})
		}
	}

	// Pull pending/failed from fix tracker (in-memory, leader only)
	if s.ext.Fixes != nil {
		for _, p := range s.ext.Fixes.Pending() {
			pendingList = append(pendingList, map[string]interface{}{
				"timestamp": p.Timestamp, "namespace": p.Namespace, "workload": p.Workload,
				"reason": p.Reason, "action": p.Action, "status": "pending",
			})
		}
		for _, f := range s.ext.Fixes.Failed() {
			failedList = append(failedList, map[string]interface{}{
				"timestamp": f.Timestamp, "namespace": f.Namespace, "workload": f.Workload,
				"reason": f.Reason, "action": f.Action, "status": "not-fixed", "detail": f.Detail,
			})
		}
	}

	writeJSON(w, map[string]interface{}{
		"fixed":        fixedList,
		"pending":      pendingList,
		"failed":       failedList,
		"fixedCount":   len(fixedList),
		"pendingCount": len(pendingList),
		"failedCount":  len(failedList),
	})
}

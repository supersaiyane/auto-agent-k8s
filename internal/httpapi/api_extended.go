package httpapi

import (
	"fmt"
	"net/http"
	"time"

	"github.com/yourorg/auto-agent/internal/kube"
)

// Extended API dependencies — set after construction.
var (
	complianceTracker *kube.ComplianceTracker
	learningMode      *kube.LearningMode
	deployTracker     *kube.DeployTracker
	dryRunLog         *kube.DryRunLog
	fixTracker        *kube.FixTracker
)

// SetExtendedDeps wires optional dependencies for extended API endpoints.
func SetExtendedDeps(ct *kube.ComplianceTracker, lm *kube.LearningMode, dt *kube.DeployTracker, dr *kube.DryRunLog, ft *kube.FixTracker) {
	complianceTracker = ct
	learningMode = lm
	deployTracker = dt
	dryRunLog = dr
	fixTracker = ft
}

func (s *Server) handleCompliance(w http.ResponseWriter, r *http.Request) {
	if complianceTracker == nil {
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
	writeJSON(w, complianceTracker.GenerateReport(since))
}

func (s *Server) handleBaselines(w http.ResponseWriter, r *http.Request) {
	if learningMode == nil {
		writeJSON(w, map[string]interface{}{"learning": false, "baselines": map[string]interface{}{}})
		return
	}
	writeJSON(w, map[string]interface{}{
		"learning":  learningMode.IsLearning(),
		"baselines": learningMode.AllBaselines(),
	})
}

func (s *Server) handleDeploys(w http.ResponseWriter, r *http.Request) {
	if deployTracker == nil {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, deployTracker.All())
}

func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	if dryRunLog == nil {
		writeJSON(w, []interface{}{})
		return
	}
	writeJSON(w, dryRunLog.Recent(100))
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
	if fixTracker != nil {
		for _, p := range fixTracker.Pending() {
			pendingList = append(pendingList, map[string]interface{}{
				"timestamp": p.Timestamp, "namespace": p.Namespace, "workload": p.Workload,
				"reason": p.Reason, "action": p.Action, "status": "pending",
			})
		}
		for _, f := range fixTracker.Failed() {
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

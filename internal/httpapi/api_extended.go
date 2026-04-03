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
)

// SetExtendedDeps wires optional dependencies for extended API endpoints.
func SetExtendedDeps(ct *kube.ComplianceTracker, lm *kube.LearningMode, dt *kube.DeployTracker, dr *kube.DryRunLog) {
	complianceTracker = ct
	learningMode = lm
	deployTracker = dt
	dryRunLog = dr
}

func (s *Server) handleCompliance(w http.ResponseWriter, r *http.Request) {
	if complianceTracker == nil {
		writeJSON(w, map[string]string{"error": "compliance tracker not initialized"})
		return
	}
	// Default: last 30 days
	since := time.Now().Add(-30 * 24 * time.Hour)
	if v := r.URL.Query().Get("days"); v != "" {
		var days int
		if _, err := fmt.Sscanf(v, "%d", &days); err == nil && days > 0 {
			since = time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		}
	}
	report := complianceTracker.GenerateReport(since)
	writeJSON(w, report)
}

func (s *Server) handleBaselines(w http.ResponseWriter, r *http.Request) {
	if learningMode == nil {
		writeJSON(w, map[string]interface{}{"learning": false, "baselines": map[string]interface{}{}})
		return
	}
	writeJSON(w, map[string]interface{}{
		"learning":   learningMode.IsLearning(),
		"baselines":  learningMode.AllBaselines(),
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

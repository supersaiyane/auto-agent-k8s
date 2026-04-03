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
	if fixTracker == nil {
		writeJSON(w, map[string]interface{}{
			"fixed": []interface{}{}, "pending": []interface{}{}, "failed": []interface{}{},
		})
		return
	}
	pending, fixed, failed := fixTracker.Stats()
	writeJSON(w, map[string]interface{}{
		"fixed":        fixTracker.Fixed(),
		"pending":      fixTracker.Pending(),
		"failed":       fixTracker.Failed(),
		"fixedCount":   fixed,
		"pendingCount": pending,
		"failedCount":  failed,
	})
}

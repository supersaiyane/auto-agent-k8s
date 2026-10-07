package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/redact"
)

// maxIngestBody bounds one forwarded batch.
const maxIngestBody = 1 << 20

// handleIngest accepts events forwarded by node agents (ADR-001). It has
// its own token, separate from the dashboard's, and stores only events
// from allowlisted namespaces, with their text redacted.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if s.internalToken == "" || s.ingest == nil {
		http.Error(w, "event ingest disabled on this pod", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.internalToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var batch []events.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBody)).Decode(&batch); err != nil {
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(batch) > events.MaxBatch {
		http.Error(w, "batch too large", http.StatusRequestEntityTooLarge)
		return
	}
	for _, e := range batch {
		if e.Namespace != "" && !s.nsAllowed(e.Namespace) {
			obs.HandlerErrorsTotal.WithLabelValues("ingest", "namespace_not_allowed").Inc()
			continue
		}
		e.ID = 0 // the controller numbers its own log
		e.Message = redact.String(e.Message)
		s.ingest.Record(e)
	}
	w.WriteHeader(http.StatusNoContent)
}

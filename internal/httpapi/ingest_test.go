package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
)

func ingestServer(t *testing.T, internal string) (*Server, *events.Recorder) {
	t.Helper()
	rec := events.NewRecorder(10)
	s := NewServer(":0", rec, &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{
		DashboardToken: "dash", InternalToken: internal,
		AllowNamespace: func(ns string) bool { return ns == "default" },
	})
	return s, rec
}

func postIngest(s *Server, method, auth, body string) int {
	r := httptest.NewRequest(method, events.IngestPath, strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	return w.Code
}

// ADR-001: node agents forward events to the controller with their own
// token; the dashboard token does not open this door, nor the reverse.
func TestIngest(t *testing.T) {
	s, rec := ingestServer(t, "node-secret")
	good := `[{"type":"incident","namespace":"default","reason":"CrashLoopBackOff","message":"db password=hunter2xyz","node":"node-a","id":99},` +
		`{"type":"incident","namespace":"payments","reason":"OOMKilled"},` +
		`{"type":"incident","reason":"EtcdNoLeader"}]`
	for _, tc := range []struct {
		name, method, auth, body string
		want                     int
	}{
		{"wrong method", "PUT", "Bearer node-secret", "", http.StatusMethodNotAllowed},
		{"export needs the token", "GET", "Bearer dash", "", http.StatusUnauthorized},
		{"no token", "POST", "", good, http.StatusUnauthorized},
		{"dashboard token", "POST", "Bearer dash", good, http.StatusUnauthorized},
		{"bad json", "POST", "Bearer node-secret", "{", http.StatusBadRequest},
		{"too large", "POST", "Bearer node-secret", "[" + strings.Repeat("{},", events.MaxBatch) + "{}]", http.StatusRequestEntityTooLarge},
		{"accepted", "POST", "Bearer node-secret", good, http.StatusNoContent},
	} {
		if got := postIngest(s, tc.method, tc.auth, tc.body); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	got := rec.Recent(10)
	if len(got) != 2 {
		t.Fatalf("one allowlisted and one cluster-scoped event stored, got %+v", got)
	}
	for _, e := range got {
		if e.Namespace == "payments" || strings.Contains(e.Message, "hunter2xyz") || e.ID == 99 {
			t.Fatalf("allowlist, redaction and numbering: %+v", e)
		}
	}
	if code := get(s, "/api/events", "Bearer node-secret"); code != http.StatusUnauthorized {
		t.Fatalf("the internal token does not open the dashboard API: %d", code)
	}
}

func TestIngest_DisabledWithoutToken(t *testing.T) {
	s, rec := ingestServer(t, "")
	if postIngest(s, "POST", "Bearer ", "[]") != http.StatusServiceUnavailable || rec.Count() != 0 {
		t.Fatal("no internal token, no ingest")
	}
}

// ISS-059: a peer can read the whole log to backfill a new controller.
func TestIngest_ExportForBackfill(t *testing.T) {
	s, rec := ingestServer(t, "node-secret")
	rec.Record(events.Event{Type: events.Incident, Reason: "first"})
	rec.Record(events.Event{Type: events.Incident, Reason: "second"})
	r := httptest.NewRequest("GET", events.IngestPath, nil)
	r.Header.Set("Authorization", "Bearer node-secret")
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "second") || strings.Index(w.Body.String(), "second") > strings.Index(w.Body.String(), "first") {
		t.Fatalf("export, newest first: %d %s", w.Code, w.Body.String())
	}
}

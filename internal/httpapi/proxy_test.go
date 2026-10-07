package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// standby builds a controller that is not the leader and proxies to target.
func standby(t *testing.T, leader func() (string, error)) *Server {
	t.Helper()
	return NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "standby"}, fake.NewSimpleClientset(), Options{
		DashboardToken: "dash", InternalToken: "node-secret", Leader: leader,
		AllowNamespace: func(string) bool { return true },
	})
}

func serve(s *Server, method, path, auth string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader("[]"))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	return w
}

// ADR-001: a standby controller answers API and ingest requests from the
// leader, so every request through the Service sees one view.
func TestStandbyProxiesToLeader(t *testing.T) {
	var seen []string
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" proxied="+r.Header.Get(proxiedHeader))
		if _, err := w.Write([]byte("from leader")); err != nil {
			t.Errorf("leader write: %v", err)
		}
	}))
	defer leader.Close()
	s := standby(t, func() (string, error) { return leader.URL, nil })

	if w := serve(s, "GET", "/api/events", "Bearer dash", nil); w.Code != 200 || w.Body.String() != "from leader" {
		t.Fatalf("api: %d %q", w.Code, w.Body.String())
	}
	if w := serve(s, "POST", events.IngestPath, "Bearer node-secret", nil); w.Body.String() != "from leader" {
		t.Fatalf("ingest goes to the leader too: %q", w.Body.String())
	}
	want := []string{"GET /api/events auth=Bearer dash proxied=1", "POST /internal/v1/events auth=Bearer node-secret proxied=1"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("leader saw %v", seen)
	}
	for _, p := range []string{"/healthz", "/metrics", "/"} {
		if w := serve(s, "GET", p, "", nil); w.Body.String() == "from leader" {
			t.Fatalf("%s is served locally", p)
		}
	}
	if w := serve(s, "GET", "/api/events", "Bearer dash", map[string]string{proxiedHeader: "1"}); w.Body.String() == "from leader" {
		t.Fatal("an already proxied request is not forwarded again")
	}
}

func TestStandbyWithoutALeader(t *testing.T) {
	s := standby(t, func() (string, error) { return "", errors.New("election running") })
	w := serve(s, "GET", "/api/events", "Bearer dash", nil)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("no leader: %d %v", w.Code, w.Header())
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	s = standby(t, func() (string, error) { return gone.URL, nil })
	if w := serve(s, "GET", "/api/events", "Bearer dash", nil); w.Code != http.StatusBadGateway {
		t.Fatalf("unreachable leader: %d", w.Code)
	}
	s = standby(t, func() (string, error) { return "http://bad host", nil })
	if w := serve(s, "GET", "/api/events", "Bearer dash", nil); w.Code != http.StatusBadGateway {
		t.Fatalf("bad leader address: %d", w.Code)
	}
}

// The leader itself, or a server with no resolver, serves locally.
func TestLeaderServesLocally(t *testing.T) {
	for name, s := range map[string]*Server{
		"leader":      standby(t, func() (string, error) { return "", nil }),
		"no resolver": standby(t, nil),
	} {
		if w := serve(s, "GET", "/api/status", "Bearer dash", nil); w.Code != http.StatusOK {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
}

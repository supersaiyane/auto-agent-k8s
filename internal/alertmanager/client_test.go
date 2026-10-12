package alertmanager

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
)

func reply(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, status, `{}`) }
}

func TestFire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{"success", reply(200), false},
		{"error status", reply(400), true},
		{"timeout", httpxtest.Slow(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(tc.handler)
			defer s.Close()
			err := New(s.URL, s.Client(100*time.Millisecond)).Fire(context.Background(), Alert{
				Labels:      map[string]string{"alertname": "X"},
				Annotations: map[string]string{"description": "token ghp_0123456789abcdefghijABCDEFGHIJ012345"},
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			r := s.Requests()
			if len(r) != 1 || r[0].Path != "/api/v2/alerts" || strings.Contains(r[0].Body, "ghp_") {
				t.Fatalf("one redacted POST to /api/v2/alerts expected: %+v", r)
			}
		})
	}
}

func TestFireIncidentAndCircuitBreaker(t *testing.T) {
	s := httpxtest.New(reply(200))
	defer s.Close()
	c := New(s.URL, s.Client(time.Second))
	c.FireIncident(context.Background(), "CrashLoopBackOff", "ns", "api", "api-1", "boom", "critical")
	c.FireCircuitBreaker(context.Background(), "ns", "api")
	r := s.Requests()
	if len(r) != 2 || !strings.Contains(r[0].Body, "AutoAgentIncident") || !strings.Contains(r[1].Body, "AutoAgentCircuitBreaker") {
		t.Fatalf("got %+v", r)
	}
	if err := New("", nil).Fire(context.Background()); err != nil {
		t.Fatal("an unconfigured client does nothing")
	}
}

// PLAN-004 phase 6 (B5): a nil client is "Alertmanager not configured" and
// does nothing; a failing Alertmanager is logged, never fatal to a handler.
func TestConvenienceAlerts_NilClientAndFailures(t *testing.T) {
	var none *Client
	none.FireIncident(context.Background(), "OOMKilled", "ns", "wl", "pod", "msg", "critical")
	none.FireCircuitBreaker(context.Background(), "ns", "wl")

	s := httpxtest.New(reply(500))
	defer s.Close()
	c := New(s.URL, s.Client(time.Second))
	c.FireIncident(context.Background(), "OOMKilled", "ns", "wl", "pod", "msg", "critical")
	c.FireCircuitBreaker(context.Background(), "ns", "wl")
	if n := len(s.Requests()); n != 2 {
		t.Fatalf("both alerts are attempted even when Alertmanager fails, got %d", n)
	}
	if err := New("http://bad host", s.Client(time.Second)).Fire(context.Background(), Alert{}); err == nil {
		t.Fatal("a URL that cannot make a request is an error")
	}
}

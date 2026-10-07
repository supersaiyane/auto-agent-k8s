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

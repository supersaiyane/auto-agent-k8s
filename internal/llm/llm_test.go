package llm

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
)

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, status, body) }
}

func TestDiagnose(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"success", reply(200, `{"choices":[{"message":{"content":"restart it"}}]}`), "restart it"},
		{"error status", reply(500, `{}`), ""},
		{"bad json", reply(200, `{`), ""},
		{"no choices", reply(200, `{"choices":[]}`), ""},
		{"timeout", httpxtest.Slow(time.Second), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(tc.handler)
			defer s.Close()
			c := New(s.URL, "key", "m", true, 5, s.Client(100*time.Millisecond))
			if got := c.Diagnose(context.Background(), "title", strings.Repeat("x", 20000)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if r := s.Requests(); len(r) != 1 || !strings.Contains(r[0].Body, "truncated") {
				t.Fatalf("one request with the long context truncated, got %d", len(r))
			}
		})
	}
}

func TestDisabledAndFallback(t *testing.T) {
	s := httpxtest.New(reply(200, `{"choices":[{"message":{"content":"check DNS"}}]}`))
	defer s.Close()
	for _, c := range []*Client{
		New(s.URL, "key", "m", false, 5, nil),
		New("", "key", "m", true, 5, nil),
		New(s.URL, "", "m", true, 5, nil),
	} {
		if c.Enabled() || c.Diagnose(context.Background(), "t", "c") != "" || c.DiagnoseWithFallback(context.Background(), "t", "c") != "" {
			t.Fatal("a disabled or unconfigured LLM does nothing")
		}
	}
	if len(s.Requests()) != 0 {
		t.Fatal("no request when disabled")
	}
	got := New(s.URL, "key", "m", true, 0, s.Client(time.Second)).DiagnoseWithFallback(context.Background(), "t", "c")
	if !strings.Contains(got, "check DNS") || !strings.Contains(got, "LLM diagnosis") {
		t.Fatalf("fallback formatting: %q", got)
	}
}

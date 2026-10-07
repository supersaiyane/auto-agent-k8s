package slack

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
)

func ok(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 200, `ok`) }

func TestPost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{"success", ok, false},
		{"error status", func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 500, `no`) }, true},
		{"timeout", httpxtest.Slow(time.Second), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(tc.handler)
			defer s.Close()
			c := New(s.URL+"/hook", 5, s.Client(100*time.Millisecond))
			if err := c.Post("hello password=hunter2xyz"); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			r := s.Requests()
			if len(r) != 1 || !strings.Contains(r[0].Body, "hello") || strings.Contains(r[0].Body, "hunter2xyz") {
				t.Fatalf("expected one redacted post, got %+v", r)
			}
		})
	}
}

func TestChannelRoutingAndBlocks(t *testing.T) {
	s := httpxtest.New(ok)
	defer s.Close()
	c := New(s.URL+"/default", 0, s.Client(time.Second))
	c.RegisterChannel("ops", s.URL+"/ops")
	steps := []error{
		c.Postf("n=%d", 3),
		c.PostToChannel("ops", "to ops"),
		c.PostToChannel("unknown", "falls back to the default hook"),
		c.PostBlocks(BuildIncidentBlocks("T", "detail", "ns", "wl", "inc-1")),
		c.PostBlocksToChannel("ops", BuildIncidentBlocks("T", "d", "ns", "wl", "i")),
	}
	for i, err := range steps {
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	var paths []string
	for _, r := range s.Requests() {
		paths = append(paths, r.Path)
	}
	if got := strings.Join(paths, ","); got != "/default,/ops,/default,/default,/ops" {
		t.Fatalf("routing: %s", got)
	}
	if !strings.Contains(s.Requests()[3].Body, "approve_fix") {
		t.Fatal("incident blocks carry the action buttons")
	}
}

func TestNoWebhookSendsNothing(t *testing.T) {
	c := New("", 5, nil)
	if err := c.Post("x"); err != nil {
		t.Fatal(err)
	}
	if err := c.PostBlocks(nil); err != nil {
		t.Fatal(err)
	}
}

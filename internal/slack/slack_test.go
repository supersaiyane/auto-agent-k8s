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
		c.PostBlocks(testBlocks),
		c.PostBlocksToChannel("ops", testBlocks),
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
	if !strings.Contains(s.Requests()[3].Body, `"blocks"`) || !strings.Contains(s.Requests()[3].Body, "detail") {
		t.Fatal("PostBlocks sends the blocks it is given")
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

// testBlocks is a minimal Block Kit message for the posting tests.
var testBlocks = []map[string]interface{}{{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": "detail"}}}

// PLAN-004 phase 6 (B5): blocks to an empty channel go to the default hook;
// a hook that is gone or answers non-200 is an error; long text is cut.
func TestBlocks_FailuresAndDefaults(t *testing.T) {
	s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	c := New(s.URL+"/default", 0, s.Client(time.Second))
	if err := c.PostBlocksToChannel("", testBlocks); err != nil {
		t.Fatalf("an empty channel uses the default hook: %v", err)
	}
	s.Close()
	if err := c.PostBlocks(testBlocks); err == nil {
		t.Fatal("a hook that is gone is an error")
	}
	bad := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	defer bad.Close()
	if err := New(bad.URL, 0, bad.Client(time.Second)).PostBlocks(testBlocks); err == nil {
		t.Fatal("a non-200 answer to blocks is an error")
	}
	if got := truncate(strings.Repeat("a", 20), 5); got != "aaaaa..." {
		t.Fatalf("truncate: %q", got)
	}
}

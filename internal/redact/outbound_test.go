package redact_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/alertmanager"
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/llm"
	"github.com/supersaiyane/auto-agent-k8s/internal/slack"
)

// Synthetic secrets that a crashing pod might print.
const leakyLog = "boot failed: password=hunter2xyz token ghp_0123456789abcdefghijABCDEFGHIJ012345 " +
	"owner alice.smith@example.com db postgres://app:s3cr3tpw@db.internal:5432/orders"

var secrets = []string{"hunter2xyz", "ghp_0123456789abcdefghijABCDEFGHIJ012345", "alice.smith@example.com", "s3cr3tpw"}

// captureTransport records every outbound request body and answers with a
// body generous enough for each client's happy path. Nothing leaves the test.
type captureTransport struct {
	mu     sync.Mutex
	bodies []string
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, r.URL.String()+"\n"+string(b))
	c.mu.Unlock()
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	resp := `{"total_count":0,"items":[],"issues":[],"number":1,"html_url":"https://x/1","web_url":"https://x/1",` +
		`"key":"OPS-1","id":"1","object":{"sha":"abc"},"commit":{"sha":"abc"},"sha":"abc",` +
		`"choices":[{"message":{"content":"ok"}}]}`
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(resp)), Request: r}, nil
}

// ISS-011, constraint 8: every client that sends text off the cluster
// redacts it. Each case must also show a redaction marker in what was sent,
// or the "no secret" check could pass because nothing was sent at all.
func TestOutboundClientsRedact(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		send func()
	}{
		{"llm diagnose", func() {
			llm.New("https://llm.test/v1/chat", "key", "m", true, 5).Diagnose(ctx, "CrashLoop "+leakyLog, leakyLog)
		}},
		{"slack post", func() { _ = slack.New("https://hooks.slack.test/x", 5).Post(leakyLog) }},
		{"slack blocks", func() {
			_ = slack.New("https://hooks.slack.test/x", 5).PostBlocks(
				slack.BuildIncidentBlocks("CrashLoop", leakyLog, "default", "api", "inc-1"))
		}},
		{"alertmanager", func() {
			_ = alertmanager.New("https://am.test").Fire(ctx, alertmanager.Alert{
				Labels:      map[string]string{"alertname": "AutoAgentIncident"},
				Annotations: map[string]string{"description": leakyLog},
			})
		}},
		{"github issue", func() {
			_, _ = integrations.NewGitHubIssues("tok", "o/r").CreateOrUpdate(ctx, "k1",
				integrations.Ticket{Title: "CrashLoop " + leakyLog, Body: leakyLog})
		}},
		{"jira issue", func() {
			_, _ = integrations.NewJira("tok", "https://jira.test", "OPS", "bot@corp.test").CreateOrUpdate(ctx, "k1",
				integrations.Ticket{Title: "CrashLoop " + leakyLog, Body: leakyLog})
		}},
		{"github pr text", func() {
			_, _ = integrations.NewGitHub("tok", "o/r", "main").OpenPR(ctx, integrations.GitOpsChange{
				FilePath: "values.yaml", Content: []byte("replicas: 2\n"), Title: "fix " + leakyLog,
				Body: leakyLog, Branch: "auto-agent/x"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := &captureTransport{}
			orig := http.DefaultTransport
			http.DefaultTransport = ct
			defer func() { http.DefaultTransport = orig }()

			tc.send()

			all := strings.Join(ct.bodies, "\n---\n")
			if len(ct.bodies) == 0 {
				t.Fatal("client sent nothing; the test cannot judge redaction")
			}
			for _, s := range secrets {
				if strings.Contains(all, s) {
					t.Errorf("%q left the cluster:\n%s", s, all)
				}
			}
			if !strings.Contains(all, "redacted") {
				t.Errorf("no redaction marker in what was sent:\n%s", all)
			}
		})
	}
}

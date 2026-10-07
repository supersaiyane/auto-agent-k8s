package integrations

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
)

// calls lists "METHOD path" for every request, without the query string.
func calls(s *httpxtest.Server) string {
	var out []string
	for _, r := range s.Requests() {
		p, _, _ := strings.Cut(r.Path, "?")
		out = append(out, r.Method+" "+p)
	}
	return strings.Join(out, ", ")
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, code, body) }
}

func TestGitHubIssues(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, search, want string }{
		{"creates when none exists", `{"total_count":0,"items":[]}`,
			"GET /search/issues, POST /repos/o/r/issues"},
		{"comments on an open issue", `{"total_count":1,"items":[{"number":7,"state":"open","html_url":"https://gh/7"}]}`,
			"GET /search/issues, POST /repos/o/r/issues/7/comments"},
		{"comments and reopens a closed issue", `{"total_count":1,"items":[{"number":7,"state":"closed","html_url":"https://gh/7"}]}`,
			"GET /search/issues, POST /repos/o/r/issues/7/comments, PATCH /repos/o/r/issues/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/search"):
					httpxtest.JSON(w, 200, tc.search)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues":
					httpxtest.JSON(w, 201, `{"number":1,"html_url":"https://gh/1"}`)
				default:
					httpxtest.JSON(w, 200, `{}`)
				}
			})
			defer s.Close()
			url, err := NewGitHubIssues("tok", "o/r", s.Client(time.Second)).CreateOrUpdate(ctx, "k1",
				Ticket{Title: "t", Body: "password=hunter2xyz", Labels: []string{"x"}})
			if err != nil || url == "" {
				t.Fatalf("url %q err %v", url, err)
			}
			if got := calls(s); got != tc.want {
				t.Fatalf("requests:\n got %s\nwant %s", got, tc.want)
			}
			for _, r := range s.Requests() {
				if strings.Contains(r.Body, "hunter2xyz") {
					t.Fatal("ticket text must be redacted")
				}
			}
		})
	}
	if _, err := NewGitHubIssues("", "o/r", nil).CreateOrUpdate(ctx, "k", Ticket{}); err == nil {
		t.Fatal("a missing token is an error")
	}
	bad := httpxtest.New(status(500, `no`))
	defer bad.Close()
	if _, err := NewGitHubIssues("tok", "o/r", bad.Client(time.Second)).CreateOrUpdate(ctx, "k", Ticket{}); err == nil {
		t.Fatal("an API error is returned")
	}
	slow := httpxtest.New(httpxtest.Slow(time.Second))
	defer slow.Close()
	if _, err := NewGitHubIssues("tok", "o/r", slow.Client(100*time.Millisecond)).CreateOrUpdate(ctx, "k", Ticket{}); err == nil {
		t.Fatal("a timeout is an error")
	}
}

func TestJira(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, search, want string }{
		{"creates", `{"total":0,"issues":[]}`, "POST /rest/api/3/search, POST /rest/api/3/issue"},
		{"comments on existing", `{"total":1,"issues":[{"key":"OPS-9"}]}`, "POST /rest/api/3/search, POST /rest/api/3/issue/OPS-9/comment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/rest/api/3/search":
					httpxtest.JSON(w, 200, tc.search)
				case "/rest/api/3/issue":
					httpxtest.JSON(w, 201, `{"key":"OPS-1"}`)
				default:
					httpxtest.JSON(w, 201, `{}`)
				}
			})
			defer s.Close()
			url, err := NewJira("tok", s.URL, "OPS", "bot@corp.test", s.Client(time.Second)).CreateOrUpdate(ctx, "k1", Ticket{Title: "t", Body: "b"})
			if err != nil || !strings.Contains(url, "/browse/OPS-") {
				t.Fatalf("url %q err %v", url, err)
			}
			if got := calls(s); got != tc.want {
				t.Fatalf("requests:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
	if _, err := NewJira("", "", "OPS", "e", nil).CreateOrUpdate(ctx, "k", Ticket{}); err == nil {
		t.Fatal("missing configuration is an error")
	}
	bad := httpxtest.New(status(500, `no`))
	defer bad.Close()
	if _, err := NewJira("tok", bad.URL, "OPS", "e", bad.Client(time.Second)).CreateOrUpdate(ctx, "k", Ticket{}); err == nil {
		t.Fatal("an API error is returned")
	}
	if url, err := NewNopTicketer().CreateOrUpdate(ctx, "k", Ticket{}); err != nil || url != "(ticketing disabled)" {
		t.Fatalf("the nop ticketer sends nothing and says so: %q %v", url, err)
	}
}

func TestGitHubPR(t *testing.T) {
	ctx := context.Background()
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			httpxtest.JSON(w, 200, `{"object":{"sha":"base123"}}`)
		case strings.HasSuffix(r.URL.Path, "/git/refs"):
			httpxtest.JSON(w, 201, `{}`)
		case strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET":
			httpxtest.JSON(w, 200, `{"sha":"file456"}`)
		case strings.Contains(r.URL.Path, "/contents/"):
			httpxtest.JSON(w, 200, `{}`)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			httpxtest.JSON(w, 201, `{"html_url":"https://gh/pull/3"}`)
		}
	})
	defer s.Close()
	url, err := NewGitHub("tok", "o/r", "main", s.Client(time.Second)).OpenPR(ctx, GitOpsChange{
		FilePath: "values.yaml", Content: []byte("replicas: 2\n"), Title: "bump", Body: "b", Branch: "auto-agent/x"})
	if err != nil || url != "https://gh/pull/3" {
		t.Fatalf("url %q err %v", url, err)
	}
	want := "GET /repos/o/r/git/ref/heads/main, POST /repos/o/r/git/refs, GET /repos/o/r/contents/values.yaml, PUT /repos/o/r/contents/values.yaml, POST /repos/o/r/pulls"
	if got := calls(s); got != want {
		t.Fatalf("requests:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(s.Requests()[3].Body, "file456") {
		t.Fatal("an existing file is updated with its sha")
	}
	if _, err := NewGitHub("", "o/r", "main", nil).OpenPR(ctx, GitOpsChange{}); err == nil {
		t.Fatal("a missing token is an error")
	}
	bad := httpxtest.New(status(404, `no`))
	defer bad.Close()
	if _, err := NewGitHub("tok", "o/r", "main", bad.Client(time.Second)).OpenPR(ctx, GitOpsChange{Branch: "b"}); err == nil {
		t.Fatal("a missing base ref is an error")
	}
}

func TestGitLabMR(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		fileStatus int
		action     string
	}{{"existing file is updated", 200, `"update"`}, {"new file is created", 404, `"create"`}} {
		t.Run(tc.name, func(t *testing.T) {
			s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "HEAD":
					w.WriteHeader(tc.fileStatus)
				case strings.HasSuffix(r.URL.Path, "/merge_requests"):
					httpxtest.JSON(w, 201, `{"web_url":"https://gl/mr/5"}`)
				default:
					httpxtest.JSON(w, 201, `{}`)
				}
			})
			defer s.Close()
			url, err := NewGitLab("tok", "grp%2Fproj", "main", s.Client(time.Second)).OpenPR(ctx, GitOpsChange{
				FilePath: "env/values.yaml", Content: []byte("a: 1\n"), Title: "bump", Body: "b"})
			if err != nil || url != "https://gl/mr/5" {
				t.Fatalf("url %q err %v (requests %s)", url, err, calls(s))
			}
			var commit string
			for _, r := range s.Requests() {
				if strings.Contains(r.Path, "/repository/commits") {
					commit = r.Body
				}
			}
			if !strings.Contains(commit, tc.action) {
				t.Fatalf("commit action: %s", commit)
			}
		})
	}
	if _, err := NewGitLab("", "p", "main", nil).OpenPR(ctx, GitOpsChange{}); err == nil {
		t.Fatal("a missing token is an error")
	}
	bad := httpxtest.New(status(500, `no`))
	defer bad.Close()
	if _, err := NewGitLab("tok", "p", "main", bad.Client(time.Second)).OpenPR(ctx, GitOpsChange{}); err == nil {
		t.Fatal("a failed branch create is an error")
	}
}

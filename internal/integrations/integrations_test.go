package integrations

import (
	"context"
	"errors"
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

// ISS-032: ReadFile returns a file from the base branch or ErrFileNotFound,
// and gitops.author is sent with the commit.
func TestGitHubReadFileAndAuthor(t *testing.T) {
	ctx := context.Background()
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/env/values.yaml") && r.Method == "GET":
			if r.URL.Query().Get("ref") != "main" {
				httpxtest.JSON(w, 400, `{}`)
				return
			}
			httpxtest.JSON(w, 200, `{"encoding":"base64","content":"YXBpOgogIHg6IDEK\n"}`)
		case strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET":
			httpxtest.JSON(w, 404, `{}`)
		case strings.HasSuffix(r.URL.Path, "/git/ref/heads/main"):
			httpxtest.JSON(w, 200, `{"object":{"sha":"abc"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			httpxtest.JSON(w, 201, `{"html_url":"https://gh/pull/4"}`)
		default:
			httpxtest.JSON(w, 201, `{}`)
		}
	})
	defer s.Close()
	g := NewGitHub("tok", "o/r", "main", s.Client(time.Second), WithAuthor("Agent", "agent@corp.test"))
	got, err := g.ReadFile(ctx, "env/values.yaml")
	if err != nil || string(got) != "api:\n  x: 1\n" {
		t.Fatalf("read: %q %v", got, err)
	}
	if _, err := g.ReadFile(ctx, "missing.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := g.OpenPR(ctx, GitOpsChange{FilePath: "new.yaml", Content: []byte("a: 1\n"), Title: "t", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	var put string
	for _, r := range s.Requests() {
		if r.Method == "PUT" {
			put = r.Body
		}
	}
	if !strings.Contains(put, `"author":{"email":"agent@corp.test","name":"Agent"}`) || !strings.Contains(put, `"committer"`) {
		t.Fatalf("the commit names the configured author: %s", put)
	}
	if strings.Contains(fmtSettings(NewGitHub("t", "r", "m", nil, WithAuthor("only-name", ""))), "only-name") {
		t.Fatal("a name without an email is ignored")
	}
}

func fmtSettings(g GitOps) string { return g.(*githubClient).authorName }

func TestGitLabReadFileAndAuthor(t *testing.T) {
	ctx := context.Background()
	s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/raw") && strings.Contains(r.URL.Path, "values.yaml"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte("api: {}\n"))
		case strings.HasSuffix(r.URL.Path, "/raw"):
			httpxtest.JSON(w, 404, `{}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			httpxtest.JSON(w, 201, `{"web_url":"https://gl/mr/1"}`)
		default:
			httpxtest.JSON(w, 201, `{}`)
		}
	})
	defer s.Close()
	g := NewGitLab("tok", "p", "main", s.Client(time.Second), WithAuthor("Agent", "agent@corp.test"))
	if got, err := g.ReadFile(ctx, "values.yaml"); err != nil || string(got) != "api: {}\n" {
		t.Fatalf("read: %q %v", got, err)
	}
	if _, err := g.ReadFile(ctx, "nope.yaml"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := g.OpenPR(ctx, GitOpsChange{FilePath: "values.yaml", Content: []byte("a"), Title: "t", Branch: "b"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range s.Requests() {
		if strings.HasSuffix(r.Path, "/commits") && strings.Contains(r.Body, `"author_email":"agent@corp.test"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the GitLab commit names the configured author")
	}
}

// ISS-012: live mode commits to the base branch with no branch and no PR.
func TestCommitToBaseBranch(t *testing.T) {
	ctx := context.Background()
	gh := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET":
			httpxtest.JSON(w, 404, `{}`)
		default:
			httpxtest.JSON(w, 200, `{"commit":{"html_url":"https://gh/commit/1"}}`)
		}
	})
	defer gh.Close()
	url, err := NewGitHub("tok", "o/r", "main", gh.Client(time.Second)).Commit(ctx, GitOpsChange{FilePath: "values.yaml", Content: []byte("a: 1\n"), Title: "bump"})
	if err != nil || url != "https://gh/commit/1" {
		t.Fatalf("github commit: %q %v", url, err)
	}
	if got := calls(gh); got != "GET /repos/o/r/contents/values.yaml, PUT /repos/o/r/contents/values.yaml" {
		t.Fatalf("github live mode touches only the file: %s", got)
	}
	if !strings.Contains(gh.Requests()[1].Body, `"branch":"main"`) {
		t.Fatal("the commit goes to the base branch")
	}
	if _, err := NewGitHub("", "o/r", "main", nil).Commit(ctx, GitOpsChange{}); err == nil {
		t.Fatal("no token is an error")
	}

	gl := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(404)
			return
		}
		httpxtest.JSON(w, 201, `{"web_url":"https://gl/commit/1"}`)
	})
	defer gl.Close()
	url, err = NewGitLab("tok", "p", "main", gl.Client(time.Second)).Commit(ctx, GitOpsChange{FilePath: "values.yaml", Content: []byte("a"), Title: "bump"})
	if err != nil || url != "https://gl/commit/1" {
		t.Fatalf("gitlab commit: %q %v", url, err)
	}
	for _, r := range gl.Requests() {
		if strings.Contains(r.Path, "merge_requests") || strings.Contains(r.Path, "branches") {
			t.Fatalf("live mode opened a branch or MR: %s", r.Path)
		}
	}
	if !strings.Contains(gl.Requests()[1].Body, `"branch":"main"`) || !strings.Contains(gl.Requests()[1].Body, `"action":"create"`) {
		t.Fatalf("gitlab commit body: %s", gl.Requests()[1].Body)
	}
	if _, err := NewGitLab("", "p", "main", nil).Commit(ctx, GitOpsChange{}); err == nil {
		t.Fatal("no token is an error")
	}
}

// PLAN-004 phase 6 (B5): a GitOps change fails cleanly at every step. A
// server that fails the nth request, one that is gone, and answers that do
// not parse each return an error, never a URL.
func TestGitOps_EveryStepCanFail(t *testing.T) {
	ctx := context.Background()
	// good answers every request the happy path makes, for both providers.
	good := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "HEAD":
			w.WriteHeader(200)
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			httpxtest.JSON(w, 200, `{"object":{"sha":"base123"}}`)
		case strings.HasSuffix(r.URL.Path, "/raw"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte("a: 1\n"))
		case strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET":
			httpxtest.JSON(w, 200, `{"sha":"file456","encoding":"base64","content":"YTogMQo="}`)
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			httpxtest.JSON(w, 201, `{"html_url":"https://gh/pull/3"}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests"):
			httpxtest.JSON(w, 201, `{"web_url":"https://gl/mr/5"}`)
		case strings.Contains(r.URL.Path, "/repository/commits"):
			httpxtest.JSON(w, 201, `{"web_url":"https://gl/commit/9"}`)
		case strings.Contains(r.URL.Path, "/contents/"):
			httpxtest.JSON(w, 200, `{"commit":{"html_url":"https://gh/commit/9"}}`)
		default:
			httpxtest.JSON(w, 201, `{}`)
		}
	}
	ch := GitOpsChange{FilePath: "env/values.yaml", Content: []byte("a: 2\n"), Title: "bump", Body: "b", Branch: "auto-agent/x"}
	ops := map[string]func(GitOps) error{
		"open":   func(g GitOps) error { _, err := g.OpenPR(ctx, ch); return err },
		"commit": func(g GitOps) error { _, err := g.Commit(ctx, ch); return err },
		"read":   func(g GitOps) error { _, err := g.ReadFile(ctx, "env/values.yaml"); return err },
	}
	providers := map[string]func(*http.Client) GitOps{
		"github": func(c *http.Client) GitOps { return NewGitHub("tok", "o/r", "main", c) },
		"gitlab": func(c *http.Client) GitOps { return NewGitLab("tok", "grp%2Fproj", "main", c) },
	}
	for pname, newClient := range providers {
		for oname, op := range ops {
			ok := httpxtest.New(good)
			if err := op(newClient(ok.Client(time.Second))); err != nil {
				t.Fatalf("%s %s on a healthy server: %v", pname, oname, err)
			}
			steps := len(ok.Requests())
			ok.Close()
			for fail := 1; fail <= steps; fail++ {
				n := 0
				s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
					n++
					if n == fail {
						httpxtest.JSON(w, 500, strings.Repeat("x", 300)) // also a body long enough to be cut
						return
					}
					good(w, r)
				})
				err := op(newClient(s.Client(time.Second)))
				s.Close()
				if err == nil {
					t.Errorf("%s %s with request %d of %d failing: want an error", pname, oname, fail, steps)
				}
			}
			gone := httpxtest.New(good)
			c := gone.Client(time.Second)
			gone.Close()
			if err := op(newClient(c)); err == nil {
				t.Errorf("%s %s with the server gone: want an error", pname, oname)
			}
		}
	}

	for name, body := range map[string]string{"not json": `nope`, "not base64": `{"encoding":"base64","content":"%%%"}`, "other encoding": `{"encoding":"none"}`} {
		b := body
		s := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 200, b) })
		_, err := NewGitHub("tok", "o/r", "main", s.Client(time.Second)).ReadFile(ctx, "x.yaml")
		s.Close()
		if err == nil {
			t.Errorf("github read, %s: want an error", name)
		}
	}
	if truncBody([]byte(strings.Repeat("y", 250))) != strings.Repeat("y", 200)+"..." || truncBody([]byte("short")) != "short" {
		t.Fatal("truncBody keeps 200 bytes and marks the cut")
	}
}

// A step whose answer cannot be read fails the change; so does a request
// that cannot even be built (a repository name with a control character).
func TestGitOps_UnreadableAnswersAndUnbuildableRequests(t *testing.T) {
	ctx := context.Background()
	ch := GitOpsChange{FilePath: "env/values.yaml", Content: []byte("a: 2\n"), Title: "bump", Branch: "auto-agent/x"}
	garbageAt := func(match func(r *http.Request) bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case match(r):
				httpxtest.JSON(w, 200, `not json`)
			case r.Method == "HEAD":
				w.WriteHeader(200)
			case strings.Contains(r.URL.Path, "/git/ref/heads/"):
				httpxtest.JSON(w, 200, `{"object":{"sha":"base"}}`)
			case strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET":
				httpxtest.JSON(w, 200, `{"sha":"f"}`)
			default:
				httpxtest.JSON(w, 201, `{"html_url":"https://gh/x","web_url":"https://gl/x","commit":{"html_url":"https://gh/c"}}`)
			}
		}
	}
	for name, tc := range map[string]struct {
		match func(*http.Request) bool
		op    func(GitOps) error
		gh    bool
	}{
		"github base ref":      {func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/git/ref/heads/") }, func(g GitOps) error { _, err := g.OpenPR(ctx, ch); return err }, true},
		"github file lookup":   {func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/contents/") && r.Method == "GET" }, func(g GitOps) error { _, err := g.Commit(ctx, ch); return err }, true},
		"github commit answer": {func(r *http.Request) bool { return r.Method == "PUT" }, func(g GitOps) error { _, err := g.Commit(ctx, ch); return err }, true},
		"github pull request":  {func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/pulls") }, func(g GitOps) error { _, err := g.OpenPR(ctx, ch); return err }, true},
		"gitlab commit answer": {func(r *http.Request) bool { return strings.Contains(r.URL.Path, "/repository/commits") }, func(g GitOps) error { _, err := g.Commit(ctx, ch); return err }, false},
		"gitlab merge request": {func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/merge_requests") }, func(g GitOps) error { _, err := g.OpenPR(ctx, ch); return err }, false},
	} {
		s := httpxtest.New(garbageAt(tc.match))
		g := NewGitLab("tok", "grp%2Fproj", "main", s.Client(time.Second))
		if tc.gh {
			g = NewGitHub("tok", "o/r", "main", s.Client(time.Second))
		}
		err := tc.op(g)
		s.Close()
		if err == nil {
			t.Errorf("%s: an answer that does not parse must be an error", name)
		}
	}

	for name, g := range map[string]GitOps{"github": NewGitHub("tok", "o/\x7fr", "main", nil), "gitlab": NewGitLab("tok", "p\x7f", "main", nil)} {
		if _, err := g.OpenPR(ctx, ch); err == nil {
			t.Errorf("%s open: an unbuildable request is an error", name)
		}
		if _, err := g.Commit(ctx, ch); err == nil {
			t.Errorf("%s commit: an unbuildable request is an error", name)
		}
		if _, err := g.ReadFile(ctx, "x"); err == nil {
			t.Errorf("%s read: an unbuildable request is an error", name)
		}
	}
}

// Tickets fail cleanly at every step too.
func TestTickets_EveryStepCanFail(t *testing.T) {
	ctx := context.Background()
	tk := Ticket{Title: "t", Body: "b", Labels: []string{"x"}, Assignees: []string{"oncall"}}
	for name, tc := range map[string]struct {
		search string
		mk     func(*http.Client) Ticketer
	}{
		"github create": {`{"total_count":0,"items":[]}`, func(c *http.Client) Ticketer { return NewGitHubIssues("tok", "o/r", c) }},
		"github reopen": {`{"total_count":1,"items":[{"number":7,"state":"closed","html_url":"https://gh/7"}]}`, func(c *http.Client) Ticketer { return NewGitHubIssues("tok", "o/r", c) }},
		"jira create":   {`{"total":0,"issues":[]}`, func(c *http.Client) Ticketer { return NewJira("tok", "https://jira.test", "OPS", "a@corp.test", c) }},
		"jira comment":  {`{"total":1,"issues":[{"key":"OPS-1"}]}`, func(c *http.Client) Ticketer { return NewJira("tok", "https://jira.test", "OPS", "a@corp.test", c) }},
	} {
		good := func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "search"):
				httpxtest.JSON(w, 200, tc.search)
			case r.Method == "POST" && (r.URL.Path == "/repos/o/r/issues" || strings.HasSuffix(r.URL.Path, "/issue")):
				httpxtest.JSON(w, 201, `{"number":1,"html_url":"https://gh/1","key":"OPS-2"}`)
			default:
				httpxtest.JSON(w, 201, `{}`)
			}
		}
		ok := httpxtest.New(good)
		if _, err := tc.mk(ok.Client(time.Second)).CreateOrUpdate(ctx, "k", tk); err != nil {
			t.Fatalf("%s on a healthy server: %v", name, err)
		}
		steps := len(ok.Requests())
		ok.Close()
		for fail := 1; fail <= steps; fail++ {
			n := 0
			s := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == fail {
					httpxtest.JSON(w, 500, `{}`)
					return
				}
				good(w, r)
			})
			_, err := tc.mk(s.Client(time.Second)).CreateOrUpdate(ctx, "k", tk)
			s.Close()
			if err == nil {
				t.Errorf("%s with request %d of %d failing: want an error", name, fail, steps)
			}
		}
		garbage := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 200, `not json`) })
		if _, err := tc.mk(garbage.Client(time.Second)).CreateOrUpdate(ctx, "k", tk); err == nil {
			t.Errorf("%s: a search answer that does not parse must be an error", name)
		}
		garbage.Close()
	}
	if _, err := NewGitHubIssues("tok", "o/\x7fr", nil).CreateOrUpdate(ctx, "k", tk); err == nil {
		t.Error("github: an unbuildable request is an error")
	}
	if _, err := NewJira("tok", "https://jira\x7f.test", "OPS", "a@corp.test", nil).CreateOrUpdate(ctx, "k", tk); err == nil {
		t.Error("jira: an unbuildable request is an error")
	}
}

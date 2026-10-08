package integrations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/redact"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpx"
)

// GitOpsChange describes a file change to propose via PR/MR.
type GitOpsChange struct {
	FilePath string // path in repo, e.g. "environments/prod/values.yaml"
	Content  []byte // full file content after change
	Title    string // PR title
	Body     string // PR description
	Branch   string // new branch name, e.g. "auto-agent/bump-memory-api-20260403"
}

// GitOps creates pull requests / merge requests for proposed changes.
type GitOps interface {
	OpenPR(ctx context.Context, ch GitOpsChange) (string, error) // returns PR URL
	// ReadFile returns a file from the base branch; ErrFileNotFound when it
	// does not exist.
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// ErrFileNotFound is ReadFile's answer for a path the repository lacks.
var ErrFileNotFound = errors.New("file not found in the repository")

// GitOpsOption configures a GitOps client.
type GitOpsOption func(*gitopsSettings)

type gitopsSettings struct{ authorName, authorEmail string }

// WithAuthor sets the commit author (gitops.author); both are needed.
func WithAuthor(name, email string) GitOpsOption {
	return func(s *gitopsSettings) { s.authorName, s.authorEmail = name, email }
}

func settingsFrom(opts []GitOpsOption) gitopsSettings {
	var s gitopsSettings
	for _, o := range opts {
		o(&s)
	}
	if s.authorName == "" || s.authorEmail == "" {
		s.authorName, s.authorEmail = "", ""
	}
	return s
}

// readBody reads at most 1 MiB of a response body.
func readBody(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return b, nil
}

// ---------- GitHub ----------

type githubClient struct {
	token  string
	repo   string // "owner/repo"
	base   string // default branch, e.g. "main"
	client *http.Client
	gitopsSettings
}

func NewGitHub(token, repo, base string, hc *http.Client, opts ...GitOpsOption) GitOps {
	return &githubClient{
		token:          token,
		repo:           repo,
		base:           base,
		client:         httpx.Client(hc, 30*time.Second),
		gitopsSettings: settingsFrom(opts),
	}
}

// ReadFile fetches path from the base branch through the contents API.
func (g *githubClient) ReadFile(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", g.repo, path, g.base)
	body, err := g.doGetOptional(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("gitops/github: read %s: %w", path, err)
	}
	if body == nil {
		return nil, ErrFileNotFound
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("gitops/github: parse %s: %w", path, err)
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("gitops/github: %s has encoding %q", path, out.Encoding)
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("gitops/github: decode %s: %w", path, err)
	}
	return data, nil
}

func (g *githubClient) OpenPR(ctx context.Context, ch GitOpsChange) (string, error) {
	// PR text is redacted; the file content is the change itself and is not (ISS-011).
	ch.Title, ch.Body = redact.String(ch.Title), redact.String(ch.Body)
	if g.token == "" {
		return "", fmt.Errorf("gitops/github: GIT_TOKEN not configured")
	}

	apiBase := "https://api.github.com"
	branch := ch.Branch
	if branch == "" {
		branch = fmt.Sprintf("auto-agent/%d", time.Now().Unix())
	}

	// 1. Get base branch SHA
	baseSHA, err := g.getRef(ctx, apiBase, g.base)
	if err != nil {
		return "", fmt.Errorf("gitops/github: get base ref: %w", err)
	}

	// 2. Create new branch from base
	if err := g.createRef(ctx, apiBase, branch, baseSHA); err != nil {
		return "", fmt.Errorf("gitops/github: create branch: %w", err)
	}

	// 3. Create or update file on new branch
	if err := g.createOrUpdateFile(ctx, apiBase, branch, ch.FilePath, ch.Content,
		fmt.Sprintf("auto-agent: %s", ch.Title)); err != nil {
		return "", fmt.Errorf("gitops/github: commit file: %w", err)
	}

	// 4. Open PR
	prURL, err := g.createPR(ctx, apiBase, branch, ch.Title, ch.Body)
	if err != nil {
		return "", fmt.Errorf("gitops/github: create PR: %w", err)
	}

	klog.Infof("gitops/github: opened PR %s", prURL)
	return prURL, nil
}

func (g *githubClient) getRef(ctx context.Context, apiBase, ref string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", apiBase, g.repo, ref)
	body, err := g.doGet(ctx, url)
	if err != nil {
		return "", err
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse ref: %w", err)
	}
	return out.Object.SHA, nil
}

func (g *githubClient) createRef(ctx context.Context, apiBase, branch, sha string) error {
	url := fmt.Sprintf("%s/repos/%s/git/refs", apiBase, g.repo)
	payload := map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	}
	_, err := g.doPost(ctx, url, payload)
	return err
}

func (g *githubClient) createOrUpdateFile(ctx context.Context, apiBase, branch, path string, content []byte, message string) error {
	url := fmt.Sprintf("%s/repos/%s/contents/%s", apiBase, g.repo, path)

	// Check if file exists to get its SHA (needed for update)
	var fileSHA string
	body, err := g.doGetOptional(ctx, url+"?ref="+branch)
	if err == nil && body != nil {
		var existing struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal(body, &existing) == nil {
			fileSHA = existing.SHA
		}
	}

	payload := map[string]interface{}{
		"message": message,
		"content": encodeBase64(content),
		"branch":  branch,
	}
	if fileSHA != "" {
		payload["sha"] = fileSHA
	}
	if g.authorName != "" {
		who := map[string]string{"name": g.authorName, "email": g.authorEmail}
		payload["author"], payload["committer"] = who, who
	}

	_, err = g.doPut(ctx, url, payload)
	return err
}

func (g *githubClient) createPR(ctx context.Context, apiBase, branch, title, body string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls", apiBase, g.repo)
	payload := map[string]string{
		"title": title,
		"body":  body,
		"head":  branch,
		"base":  g.base,
	}
	respBody, err := g.doPost(ctx, url, payload)
	if err != nil {
		return "", err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse PR response: %w", err)
	}
	return out.HTMLURL, nil
}

func (g *githubClient) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

func (g *githubClient) doGetOptional(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

func (g *githubClient) doPost(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	return g.doRequest(ctx, "POST", url, payload)
}

func (g *githubClient) doPut(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	return g.doRequest(ctx, "PUT", url, payload)
}

func (g *githubClient) doRequest(ctx context.Context, method, url string, payload interface{}) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

// ---------- GitLab ----------

type gitlabClient struct {
	token   string
	project string // URL-encoded project path or numeric ID
	base    string
	baseURL string // e.g. "https://gitlab.com"
	client  *http.Client
	gitopsSettings
}

func NewGitLab(token, project, base string, hc *http.Client, opts ...GitOpsOption) GitOps {
	return &gitlabClient{
		gitopsSettings: settingsFrom(opts),
		token:   token,
		project: project,
		base:    base,
		baseURL: "https://gitlab.com",
		client:  httpx.Client(hc, 30*time.Second),
	}
}

func (g *gitlabClient) OpenPR(ctx context.Context, ch GitOpsChange) (string, error) {
	// PR text is redacted; the file content is the change itself and is not (ISS-011).
	ch.Title, ch.Body = redact.String(ch.Title), redact.String(ch.Body)
	if g.token == "" {
		return "", fmt.Errorf("gitops/gitlab: GIT_TOKEN not configured")
	}

	apiBase := g.baseURL + "/api/v4"
	branch := ch.Branch
	if branch == "" {
		branch = fmt.Sprintf("auto-agent/%d", time.Now().Unix())
	}

	// 1. Create branch
	branchURL := fmt.Sprintf("%s/projects/%s/repository/branches", apiBase, g.project)
	_, err := g.doPost(ctx, branchURL, map[string]string{
		"branch": branch,
		"ref":    g.base,
	})
	if err != nil {
		return "", fmt.Errorf("gitops/gitlab: create branch: %w", err)
	}

	// 2. Commit file change
	commitURL := fmt.Sprintf("%s/projects/%s/repository/commits", apiBase, g.project)
	action := "update"
	// Check if file exists
	fileCheckURL := fmt.Sprintf("%s/projects/%s/repository/files/%s?ref=%s",
		apiBase, g.project, urlEncodePath(ch.FilePath), branch)
	req, err := http.NewRequestWithContext(ctx, "HEAD", fileCheckURL, nil)
	if err != nil {
		return "", fmt.Errorf("gitlab: file check request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	resp, err := g.client.Do(req)
	if err != nil || resp.StatusCode == 404 {
		action = "create"
	}
	if resp != nil {
		resp.Body.Close()
	}

	commitPayload := map[string]interface{}{
		"branch":         branch,
		"commit_message": fmt.Sprintf("auto-agent: %s", ch.Title),
		"actions": []map[string]string{
			{
				"action":    action,
				"file_path": ch.FilePath,
				"content":   string(ch.Content),
			},
		},
	}
	if g.authorName != "" {
		commitPayload["author_name"], commitPayload["author_email"] = g.authorName, g.authorEmail
	}
	_, err = g.doPost(ctx, commitURL, commitPayload)
	if err != nil {
		return "", fmt.Errorf("gitops/gitlab: commit: %w", err)
	}

	// 3. Create merge request
	mrURL := fmt.Sprintf("%s/projects/%s/merge_requests", apiBase, g.project)
	mrBody, err := g.doPost(ctx, mrURL, map[string]string{
		"source_branch": branch,
		"target_branch": g.base,
		"title":         ch.Title,
		"description":   ch.Body,
	})
	if err != nil {
		return "", fmt.Errorf("gitops/gitlab: create MR: %w", err)
	}

	var out struct {
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(mrBody, &out); err != nil {
		return "", fmt.Errorf("parse MR response: %w", err)
	}

	klog.Infof("gitops/gitlab: opened MR %s", out.WebURL)
	return out.WebURL, nil
}

// ReadFile fetches path from the base branch as raw bytes.
func (g *gitlabClient) ReadFile(ctx context.Context, path string) ([]byte, error) {
	url := fmt.Sprintf("%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s", g.baseURL, g.project, urlEncodePath(path), g.base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitops/gitlab: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrFileNotFound
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitops/gitlab: read %s: %d %s", path, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

func (g *gitlabClient) doPost(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s: %d %s", url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

// ---------- Helpers ----------

func truncBody(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func urlEncodePath(s string) string {
	// Minimal URL encoding for GitLab file paths (replace / with %2F)
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, '%', '2', 'F')
		} else {
			out = append(out, s[i])
		}
	}
	return string(out)
}

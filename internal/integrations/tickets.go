package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Ticket describes an issue/ticket to create or update.
type Ticket struct {
	Title     string
	Body      string
	Labels    []string
	Assignees []string
}

// Ticketer creates or updates issues in external tracking systems.
type Ticketer interface {
	// CreateOrUpdate finds an existing issue by key (dedup), or creates a new one.
	// Returns the issue URL.
	CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error)
}

// ---------- GitHub Issues ----------

type githubIssues struct {
	token  string
	repo   string // "owner/repo"
	client *http.Client
}

func NewGitHubIssues(token, repo string) Ticketer {
	return &githubIssues{
		token:  token,
		repo:   repo,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *githubIssues) CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error) {
	if g.token == "" {
		return "", fmt.Errorf("tickets/github: GITHUB_TOKEN not configured")
	}

	apiBase := "https://api.github.com"

	// Search for existing issue by key in title
	existing, err := g.findByKey(ctx, apiBase, key)
	if err != nil {
		klog.V(3).Infof("tickets/github: search failed: %v", err)
	}

	if existing != nil {
		// Update existing issue with a comment
		commentURL := fmt.Sprintf("%s/repos/%s/issues/%d/comments", apiBase, g.repo, existing.Number)
		comment := fmt.Sprintf("**Auto-agent update** (%s)\n\n%s", time.Now().UTC().Format(time.RFC3339), t.Body)
		_, err := g.doPost(ctx, commentURL, map[string]string{"body": comment})
		if err != nil {
			return "", fmt.Errorf("tickets/github: add comment: %w", err)
		}

		// Reopen if closed
		if existing.State == "closed" {
			patchURL := fmt.Sprintf("%s/repos/%s/issues/%d", apiBase, g.repo, existing.Number)
			_, err := g.doPatch(ctx, patchURL, map[string]string{"state": "open"})
			if err != nil {
				klog.Warningf("tickets/github: failed to reopen #%d: %v", existing.Number, err)
			}
		}

		klog.Infof("tickets/github: updated issue #%d", existing.Number)
		return existing.HTMLURL, nil
	}

	// Create new issue
	title := fmt.Sprintf("[auto-agent] %s", t.Title)
	body := fmt.Sprintf("<!-- auto-agent-key: %s -->\n\n%s", key, t.Body)
	payload := map[string]interface{}{
		"title": title,
		"body":  body,
	}
	if len(t.Labels) > 0 {
		payload["labels"] = t.Labels
	}
	if len(t.Assignees) > 0 {
		payload["assignees"] = t.Assignees
	}

	createURL := fmt.Sprintf("%s/repos/%s/issues", apiBase, g.repo)
	respBody, err := g.doPost(ctx, createURL, payload)
	if err != nil {
		return "", fmt.Errorf("tickets/github: create issue: %w", err)
	}

	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse issue response: %w", err)
	}

	klog.Infof("tickets/github: created issue #%d: %s", out.Number, out.HTMLURL)
	return out.HTMLURL, nil
}

type ghIssue struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

func (g *githubIssues) findByKey(ctx context.Context, apiBase, key string) (*ghIssue, error) {
	// Search issues in the repo for the auto-agent key marker
	searchURL := fmt.Sprintf("%s/search/issues?q=repo:%s+%%22auto-agent-key:+%s%%22+in:body&per_page=1",
		apiBase, g.repo, key)
	body, err := g.doGet(ctx, searchURL)
	if err != nil {
		return nil, err
	}
	var result struct {
		TotalCount int       `json:"total_count"`
		Items      []ghIssue `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.TotalCount == 0 || len(result.Items) == 0 {
		return nil, nil
	}
	return &result.Items[0], nil
}

func (g *githubIssues) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	g.setHeaders(req)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %d %s", url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

func (g *githubIssues) doPost(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	return g.doMutate(ctx, "POST", url, payload)
}

func (g *githubIssues) doPatch(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	return g.doMutate(ctx, "PATCH", url, payload)
}

func (g *githubIssues) doMutate(ctx context.Context, method, url string, payload interface{}) ([]byte, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	g.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

func (g *githubIssues) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

// ---------- Jira ----------

type jiraClient struct {
	token   string
	email   string
	baseURL string // e.g. "https://yourorg.atlassian.net"
	project string // project key, e.g. "OPS"
	client  *http.Client
}

func NewJira(token, baseURL, project, email string) Ticketer {
	return &jiraClient{
		token:   token,
		email:   email,
		baseURL: strings.TrimRight(baseURL, "/"),
		project: project,
		client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (j *jiraClient) CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error) {
	if j.token == "" || j.baseURL == "" {
		return "", fmt.Errorf("tickets/jira: JIRA_TOKEN or JIRA_BASE_URL not configured")
	}

	// Search for existing issue by the dedup key in summary
	existing, err := j.findByKey(ctx, key)
	if err != nil {
		klog.V(3).Infof("tickets/jira: search failed: %v", err)
	}

	if existing != nil {
		// Add comment to existing issue
		commentURL := fmt.Sprintf("%s/rest/api/3/issue/%s/comment", j.baseURL, existing.Key)
		comment := map[string]interface{}{
			"body": map[string]interface{}{
				"type":    "doc",
				"version": 1,
				"content": []map[string]interface{}{
					{
						"type": "paragraph",
						"content": []map[string]interface{}{
							{
								"type": "text",
								"text": fmt.Sprintf("Auto-agent update (%s): %s", time.Now().UTC().Format(time.RFC3339), t.Body),
							},
						},
					},
				},
			},
		}
		_, err := j.doPost(ctx, commentURL, comment)
		if err != nil {
			return "", fmt.Errorf("tickets/jira: add comment: %w", err)
		}

		klog.Infof("tickets/jira: updated %s", existing.Key)
		return fmt.Sprintf("%s/browse/%s", j.baseURL, existing.Key), nil
	}

	// Create new issue
	payload := map[string]interface{}{
		"fields": map[string]interface{}{
			"project":  map[string]string{"key": j.project},
			"issuetype": map[string]string{"name": "Bug"},
			"summary":  fmt.Sprintf("[auto-agent:%s] %s", key, t.Title),
			"description": map[string]interface{}{
				"type":    "doc",
				"version": 1,
				"content": []map[string]interface{}{
					{
						"type": "paragraph",
						"content": []map[string]interface{}{
							{"type": "text", "text": t.Body},
						},
					},
				},
			},
		},
	}
	if len(t.Labels) > 0 {
		payload["fields"].(map[string]interface{})["labels"] = t.Labels
	}

	createURL := fmt.Sprintf("%s/rest/api/3/issue", j.baseURL)
	respBody, err := j.doPost(ctx, createURL, payload)
	if err != nil {
		return "", fmt.Errorf("tickets/jira: create issue: %w", err)
	}

	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("parse Jira response: %w", err)
	}

	issueURL := fmt.Sprintf("%s/browse/%s", j.baseURL, out.Key)
	klog.Infof("tickets/jira: created %s: %s", out.Key, issueURL)
	return issueURL, nil
}

type jiraIssue struct {
	Key string `json:"key"`
}

func (j *jiraClient) findByKey(ctx context.Context, key string) (*jiraIssue, error) {
	searchURL := fmt.Sprintf("%s/rest/api/3/search", j.baseURL)
	jql := fmt.Sprintf(`project = "%s" AND summary ~ "auto-agent:%s" AND status != Done ORDER BY created DESC`, j.project, key)
	payload := map[string]interface{}{
		"jql":        jql,
		"maxResults": 1,
		"fields":     []string{"key", "summary", "status"},
	}
	body, err := j.doPost(ctx, searchURL, payload)
	if err != nil {
		return nil, err
	}
	var result struct {
		Total  int         `json:"total"`
		Issues []jiraIssue `json:"issues"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.Total == 0 || len(result.Issues) == 0 {
		return nil, nil
	}
	return &result.Issues[0], nil
}

func (j *jiraClient) doPost(ctx context.Context, url string, payload interface{}) ([]byte, error) {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(j.email, j.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("POST %s: %d %s", url, resp.StatusCode, truncBody(body))
	}
	return body, nil
}

// ---------- Nop ticketer (for disabled mode) ----------

type nopTicketer struct{}

func NewNopTicketer() Ticketer { return &nopTicketer{} }

func (n *nopTicketer) CreateOrUpdate(_ context.Context, _ string, _ Ticket) (string, error) {
	return "(ticketing disabled)", nil
}

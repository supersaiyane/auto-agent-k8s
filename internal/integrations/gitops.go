package integrations

import (
    "context"
    "fmt"
)

type GitOpsChange struct {
    FilePath string
    Content  []byte
    Title    string
    Body     string
    Branch   string
}

type GitOps interface {
    OpenPR(ctx context.Context, ch GitOpsChange) (string, error) // returns PR URL
}

type githubClient struct {
    token, repo, base string
}
func NewGitHub(token, repo, base string) GitOps { return &githubClient{token: token, repo: repo, base: base} }

func (g *githubClient) OpenPR(ctx context.Context, ch GitOpsChange) (string, error) {
    // Placeholder: implement full GitHub API calls (create branch, commit blob/tree, open PR).
    return fmt.Sprintf("https://github.com/%s/pull/123 (stub)", g.repo), nil
}

type gitlabClient struct { token, repo, base string }
func NewGitLab(token, repo, base string) GitOps { return &gitlabClient{token: token, repo: repo, base: base} }
func (g *gitlabClient) OpenPR(ctx context.Context, ch GitOpsChange) (string, error) {
    return fmt.Sprintf("https://gitlab.com/%s/-/merge_requests/1 (stub)", g.repo), nil
}

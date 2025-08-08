package integrations

import "context"

type Ticket struct {
    Title string
    Body  string
    Labels []string
    Assignees []string
}

type Ticketer interface {
    CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error) // returns URL
}

type githubIssues struct { token, repo string }
func NewGitHubIssues(token, repo string) Ticketer { return &githubIssues{token: token, repo: repo} }
func (g *githubIssues) CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error) {
    // Placeholder: search by title key, create or update.
    return "https://github.com/"+g.repo+"/issues/999 (stub)", nil
}

type jiraClient struct { token, base, project, email string }
func NewJira(token, base, project, email string) Ticketer { return &jiraClient{token: token, base: base, project: project, email: email} }
func (j *jiraClient) CreateOrUpdate(ctx context.Context, key string, t Ticket) (string, error) {
    return j.base + "/browse/" + j.project + "-123 (stub)", nil
}

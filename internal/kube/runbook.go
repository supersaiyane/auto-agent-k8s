package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// RunbookStep is a single step in a runbook.
type RunbookStep struct {
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`     // "check", "action", "notify"
	Command     string `json:"command" yaml:"command"` // kubectl-style command to execute
	Expect      string `json:"expect" yaml:"expect"`   // expected output substring
	Description string `json:"description" yaml:"description"`
}

// Runbook is a sequence of diagnostic/remediation steps.
type Runbook struct {
	Name  string        `json:"name" yaml:"name"`
	Steps []RunbookStep `json:"steps" yaml:"steps"`
}

// RunbookResult is the outcome of executing a runbook.
type RunbookResult struct {
	Runbook  string             `json:"runbook"`
	Steps    []RunbookStepResult `json:"steps"`
	Success  bool               `json:"success"`
	Duration string             `json:"duration"`
}

type RunbookStepResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // "pass", "fail", "skip"
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

// FetchRunbook downloads a runbook from a URL (JSON format).
func FetchRunbook(ctx context.Context, url string) (*Runbook, error) {
	if url == "" {
		return nil, fmt.Errorf("no runbook URL")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("runbook: create request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runbook: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("runbook: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("runbook: read: %w", err)
	}
	var rb Runbook
	if err := json.Unmarshal(body, &rb); err != nil {
		return nil, fmt.Errorf("runbook: parse: %w", err)
	}
	return &rb, nil
}

// ExecuteRunbook runs the steps of a runbook using the kubectl API.
// Only read-only commands are executed for safety.
func ExecuteRunbook(ctx context.Context, deps *Deps, rb *Runbook, ns string) RunbookResult {
	start := time.Now()
	result := RunbookResult{Runbook: rb.Name, Success: true}

	for _, step := range rb.Steps {
		stepResult := RunbookStepResult{Name: step.Name}

		// Only allow read-only commands
		if !isReadOnlyCommand(step.Command) {
			stepResult.Status = "skip"
			stepResult.Output = "skipped: write operations not allowed in runbook automation"
			result.Steps = append(result.Steps, stepResult)
			continue
		}

		// Execute by calling the agent's own kubectl API
		output, err := execRunbookCmd(ctx, step.Command)
		if err != nil {
			stepResult.Status = "fail"
			stepResult.Error = err.Error()
			result.Success = false
		} else {
			stepResult.Output = output
			// Check expectation
			if step.Expect != "" && !strings.Contains(output, step.Expect) {
				stepResult.Status = "fail"
				stepResult.Error = fmt.Sprintf("expected %q not found in output", step.Expect)
				result.Success = false
			} else {
				stepResult.Status = "pass"
			}
		}
		result.Steps = append(result.Steps, stepResult)
	}

	result.Duration = time.Since(start).String()
	return result
}

// RunRunbookForIncident fetches and executes a runbook for an incident.
func RunRunbookForIncident(ctx context.Context, deps *Deps, runbookURL, ns, workload, reason string) string {
	if runbookURL == "" {
		return ""
	}

	rb, err := FetchRunbook(ctx, runbookURL)
	if err != nil {
		klog.V(3).Infof("runbook: fetch failed for %s: %v", runbookURL, err)
		return ""
	}

	result := ExecuteRunbook(ctx, deps, rb, ns)

	// Record as event
	status := "passed"
	if !result.Success {
		status = "failed"
	}
	recordEvent(deps, eventsvc.Event{
		Type: eventsvc.Action, Severity: eventsvc.SevInfo,
		Namespace: ns, Workload: workload,
		Reason:  "RunbookExecuted",
		Message: fmt.Sprintf("Runbook %q %s (%d steps, %s)", rb.Name, status, len(result.Steps), result.Duration),
		Action:  "runbook",
	})
	obs.ActionsTotal.WithLabelValues("runbook", ns, workload).Inc()

	// Format output for Slack
	msg := fmt.Sprintf("\n_Runbook_ `%s` (%s):\n", rb.Name, result.Duration)
	for _, s := range result.Steps {
		icon := "pass"
		if s.Status == "fail" {
			icon = "FAIL"
		} else if s.Status == "skip" {
			icon = "skip"
		}
		msg += fmt.Sprintf("  [%s] %s", icon, s.Name)
		if s.Error != "" {
			msg += fmt.Sprintf(" — %s", s.Error)
		}
		msg += "\n"
	}
	return msg
}

// execRunbookCmd executes a command by calling the agent's own API.
func execRunbookCmd(ctx context.Context, cmd string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	payload := fmt.Sprintf(`{"command":"%s"}`, strings.ReplaceAll(cmd, `"`, `\"`))
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost:8080/api/kubectl",
		strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var result struct {
		Output string `json:"output"`
		Error  string `json:"error"`
	}
	json.Unmarshal(body, &result)
	if result.Error != "" {
		return result.Output, fmt.Errorf("%s", result.Error)
	}
	return result.Output, nil
}

// isReadOnlyCommand checks if a kubectl command is read-only (safe to auto-execute).
func isReadOnlyCommand(cmd string) bool {
	cmd = strings.TrimPrefix(strings.TrimSpace(cmd), "kubectl ")
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return false
	}
	readOnly := map[string]bool{
		"get": true, "describe": true, "logs": true,
		"top": true, "version": true, "cluster-info": true,
	}
	return readOnly[parts[0]]
}

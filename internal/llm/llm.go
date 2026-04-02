package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

type Client struct {
	url     string
	key     string
	model   string
	enabled bool
	client  *http.Client
}

func New(url, key, model string, enabled bool, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	return &Client{
		url:     url,
		key:     key,
		model:   model,
		enabled: enabled,
		client: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
	}
}

func (c *Client) Enabled() bool {
	return c.enabled && c.url != "" && c.key != ""
}

// Diagnose sends logs and context to the LLM for root cause analysis.
// Returns concise SRE advice or empty string on failure.
func (c *Client) Diagnose(ctx context.Context, title, diagContext string) string {
	if !c.Enabled() {
		return ""
	}

	// Truncate context to avoid excessive token usage
	const maxCtxLen = 4000
	if len(diagContext) > maxCtxLen {
		diagContext = diagContext[:maxCtxLen] + "\n... (truncated)"
	}

	payload := map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are an SRE assistant. Diagnose the Kubernetes issue and suggest a fix. Be concise (3-5 sentences max)."},
			{"role": "user", "content": title + "\n\n" + diagContext},
		},
		"max_tokens": 200,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		klog.V(2).Infof("llm: marshal error: %v", err)
		return ""
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(b))
	if err != nil {
		klog.V(2).Infof("llm: request error: %v", err)
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		klog.Warningf("llm: request failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		klog.Warningf("llm: non-200 response %d: %s", resp.StatusCode, string(body))
		return ""
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		klog.Warningf("llm: decode error: %v", err)
		return ""
	}
	if len(out.Choices) > 0 {
		return out.Choices[0].Message.Content
	}
	return ""
}

// DiagnoseWithFallback calls Diagnose and wraps the result for Slack formatting.
func (c *Client) DiagnoseWithFallback(ctx context.Context, title, diagContext string) string {
	advice := c.Diagnose(ctx, title, diagContext)
	if advice == "" {
		return ""
	}
	return fmt.Sprintf("\n_LLM diagnosis_: %s\n", advice)
}

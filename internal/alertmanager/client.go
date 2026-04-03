package alertmanager

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

// Alert represents a Prometheus Alertmanager alert.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt,omitempty"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
	GeneratorURL string           `json:"generatorURL,omitempty"`
}

// Client sends alerts to Alertmanager.
type Client struct {
	url    string // e.g. "http://alertmanager:9093"
	client *http.Client
}

func New(url string) *Client {
	if url == "" {
		return nil
	}
	return &Client{
		url: url,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Fire sends one or more alerts to Alertmanager.
func (c *Client) Fire(ctx context.Context, alerts ...Alert) error {
	if c == nil || c.url == "" {
		return nil
	}

	b, err := json.Marshal(alerts)
	if err != nil {
		return fmt.Errorf("alertmanager: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.url+"/api/v2/alerts", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("alertmanager: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("alertmanager: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("alertmanager: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// FireIncident is a convenience method for sending a remediation incident alert.
func (c *Client) FireIncident(ctx context.Context, reason, namespace, workload, pod, message, severity string) {
	if c == nil {
		return
	}
	alert := Alert{
		Labels: map[string]string{
			"alertname": "AutoAgentIncident",
			"reason":    reason,
			"namespace": namespace,
			"workload":  workload,
			"severity":  severity,
		},
		Annotations: map[string]string{
			"summary":     fmt.Sprintf("[auto-agent] %s in %s/%s", reason, namespace, workload),
			"description": message,
			"pod":         pod,
		},
		StartsAt: time.Now().UTC(),
	}
	if err := c.Fire(ctx, alert); err != nil {
		klog.Warningf("alertmanager: failed to fire %s alert: %v", reason, err)
	}
}

// FireCircuitBreaker sends a circuit breaker alert (high urgency).
func (c *Client) FireCircuitBreaker(ctx context.Context, namespace, workload string) {
	if c == nil {
		return
	}
	alert := Alert{
		Labels: map[string]string{
			"alertname": "AutoAgentCircuitBreaker",
			"namespace": namespace,
			"workload":  workload,
			"severity":  "critical",
		},
		Annotations: map[string]string{
			"summary":     fmt.Sprintf("[auto-agent] Circuit breaker tripped for %s/%s", namespace, workload),
			"description": "Too many remediation actions on this workload within the last hour. Manual investigation required.",
		},
		StartsAt: time.Now().UTC(),
	}
	if err := c.Fire(ctx, alert); err != nil {
		klog.Warningf("alertmanager: failed to fire circuit breaker alert: %v", err)
	}
}

package escalation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Severity levels for escalation routing.
type Severity string

const (
	SevCritical Severity = "critical"
	SevWarning  Severity = "warning"
	SevInfo     Severity = "info"
)

// Incident represents an incident to be escalated.
type Incident struct {
	Title     string
	Body      string
	Severity  Severity
	Namespace string
	Workload  string
	Source    string // "auto-agent"
}

// Chain sends incidents to multiple channels based on severity.
type Chain struct {
	pagerduty *PagerDutyClient
	opsgenie  *OpsGenieClient
	email     *EmailClient
}

func NewChain() *Chain {
	c := &Chain{}
	if key := os.Getenv("PAGERDUTY_ROUTING_KEY"); key != "" {
		c.pagerduty = NewPagerDuty(key)
		klog.Infof("escalation: PagerDuty configured")
	}
	if key := os.Getenv("OPSGENIE_API_KEY"); key != "" {
		c.opsgenie = NewOpsGenie(key)
		klog.Infof("escalation: OpsGenie configured")
	}
	if host := os.Getenv("SMTP_HOST"); host != "" {
		c.email = NewEmail(host, os.Getenv("SMTP_PORT"), os.Getenv("SMTP_USER"),
			os.Getenv("SMTP_PASS"), os.Getenv("SMTP_FROM"), os.Getenv("ESCALATION_EMAIL_TO"))
		klog.Infof("escalation: email configured (to: %s)", os.Getenv("ESCALATION_EMAIL_TO"))
	}
	return c
}

func (c *Chain) Configured() bool {
	return c.pagerduty != nil || c.opsgenie != nil || c.email != nil
}

// Escalate sends an incident to all configured channels.
func (c *Chain) Escalate(ctx context.Context, inc Incident) {
	if c.pagerduty != nil && (inc.Severity == SevCritical || inc.Severity == SevWarning) {
		if err := c.pagerduty.Trigger(ctx, inc); err != nil {
			klog.Warningf("escalation: pagerduty failed: %v", err)
		}
	}
	if c.opsgenie != nil && (inc.Severity == SevCritical || inc.Severity == SevWarning) {
		if err := c.opsgenie.Create(ctx, inc); err != nil {
			klog.Warningf("escalation: opsgenie failed: %v", err)
		}
	}
	if c.email != nil && inc.Severity == SevCritical {
		if err := c.email.Send(inc); err != nil {
			klog.Warningf("escalation: email failed: %v", err)
		}
	}
}

// --- PagerDuty (Events API v2) ---

type PagerDutyClient struct {
	routingKey string
	client     *http.Client
}

func NewPagerDuty(routingKey string) *PagerDutyClient {
	return &PagerDutyClient{routingKey: routingKey, client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *PagerDutyClient) Trigger(ctx context.Context, inc Incident) error {
	payload := map[string]interface{}{
		"routing_key":  p.routingKey,
		"event_action": "trigger",
		"payload": map[string]interface{}{
			"summary":   fmt.Sprintf("[auto-agent] %s", inc.Title),
			"source":    "auto-agent",
			"severity":  pdSeverity(inc.Severity),
			"component": inc.Workload,
			"group":     inc.Namespace,
			"custom_details": map[string]string{
				"namespace": inc.Namespace,
				"workload":  inc.Workload,
				"body":      truncate(inc.Body, 500),
			},
		},
	}
	return postJSON(ctx, p.client, "https://events.pagerduty.com/v2/enqueue", payload)
}

func pdSeverity(s Severity) string {
	switch s {
	case SevCritical:
		return "critical"
	case SevWarning:
		return "warning"
	default:
		return "info"
	}
}

// --- OpsGenie ---

type OpsGenieClient struct {
	apiKey string
	client *http.Client
}

func NewOpsGenie(apiKey string) *OpsGenieClient {
	return &OpsGenieClient{apiKey: apiKey, client: &http.Client{Timeout: 10 * time.Second}}
}

func (o *OpsGenieClient) Create(ctx context.Context, inc Incident) error {
	payload := map[string]interface{}{
		"message":     fmt.Sprintf("[auto-agent] %s", inc.Title),
		"description": truncate(inc.Body, 1000),
		"priority":    ogPriority(inc.Severity),
		"tags":        []string{"auto-agent", inc.Namespace, inc.Workload},
		"details": map[string]string{
			"namespace": inc.Namespace,
			"workload":  inc.Workload,
		},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.opsgenie.com/v2/alerts", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "GenieKey "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("opsgenie: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func ogPriority(s Severity) string {
	switch s {
	case SevCritical:
		return "P1"
	case SevWarning:
		return "P3"
	default:
		return "P5"
	}
}

// --- Email (SMTP) ---

type EmailClient struct {
	host, port, user, pass, from, to string
}

func NewEmail(host, port, user, pass, from, to string) *EmailClient {
	if port == "" {
		port = "587"
	}
	return &EmailClient{host: host, port: port, user: user, pass: pass, from: from, to: to}
}

func (e *EmailClient) Send(inc Incident) error {
	subject := fmt.Sprintf("[auto-agent][%s] %s", strings.ToUpper(string(inc.Severity)), inc.Title)
	body := fmt.Sprintf("Subject: %s\r\nFrom: %s\r\nTo: %s\r\nContent-Type: text/plain\r\n\r\n%s\n\nNamespace: %s\nWorkload: %s",
		subject, e.from, e.to, inc.Body, inc.Namespace, inc.Workload)

	addr := e.host + ":" + e.port
	var auth smtp.Auth
	if e.user != "" {
		auth = smtp.PlainAuth("", e.user, e.pass, e.host)
	}
	return smtp.SendMail(addr, auth, e.from, strings.Split(e.to, ","), []byte(body))
}

// --- Helpers ---

func postJSON(ctx context.Context, client *http.Client, url string, payload interface{}) error {
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

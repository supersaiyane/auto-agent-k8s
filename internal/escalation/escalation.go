package escalation

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpx"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/redact"
)

const (
	channelTimeout = 10 * time.Second // per channel, SMTP included
	maxInFlight    = 4                // escalations sent at once; more are dropped and counted
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
	inFlight  chan struct{} // bounds Send
	wg        sync.WaitGroup
}

// NewChain builds the escalation targets that are configured.
func NewChain(cfg config.Escalation, hc *http.Client) *Chain {
	c := &Chain{inFlight: make(chan struct{}, maxInFlight)}
	if key := cfg.PagerDutyRoutingKey; key != "" {
		c.pagerduty = NewPagerDuty(key, hc)
		klog.Infof("escalation: PagerDuty configured")
	}
	if key := cfg.OpsGenieAPIKey; key != "" {
		c.opsgenie = NewOpsGenie(key, hc)
		klog.Infof("escalation: OpsGenie configured")
	}
	if host := cfg.SMTPHost; host != "" {
		c.email = NewEmail(host, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPFrom, cfg.EmailTo)
		klog.Infof("escalation: email configured")
	}
	return c
}

// Configured reports whether any channel is set; a nil chain has none.
func (c *Chain) Configured() bool {
	return c != nil && (c.pagerduty != nil || c.opsgenie != nil || c.email != nil)
}

// Escalate sends an incident to every configured channel its severity
// reaches, each bounded by channelTimeout. A failed channel is logged and
// counted, and the failures are returned together.
func (c *Chain) Escalate(ctx context.Context, inc Incident) error {
	if !c.Configured() {
		return nil
	}
	pages := inc.Severity == SevCritical || inc.Severity == SevWarning
	var errs []error
	try := func(channel string, enabled bool, send func(context.Context) error) {
		if !enabled {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, channelTimeout)
		defer cancel()
		if err := send(cctx); err != nil {
			klog.Warningf("escalation: %s failed: %v", channel, err)
			obs.HandlerErrorsTotal.WithLabelValues("escalation", channel).Inc()
			errs = append(errs, fmt.Errorf("%s: %w", channel, err))
		}
	}
	try("pagerduty", c.pagerduty != nil && pages, func(ctx context.Context) error { return c.pagerduty.Trigger(ctx, inc) })
	try("opsgenie", c.opsgenie != nil && pages, func(ctx context.Context) error { return c.opsgenie.Create(ctx, inc) })
	try("email", c.email != nil && inc.Severity == SevCritical, func(ctx context.Context) error { return c.email.Send(ctx, inc) })
	return errors.Join(errs...)
}

// Send escalates in the background so a slow channel never holds up the
// caller. At most maxInFlight run at once; beyond that the incident is
// dropped, logged and counted rather than queued without bound.
func (c *Chain) Send(inc Incident) {
	if !c.Configured() {
		return
	}
	select {
	case c.inFlight <- struct{}{}:
	default:
		klog.Warningf("escalation: %d already in flight, dropped %q", maxInFlight, inc.Title)
		obs.HandlerErrorsTotal.WithLabelValues("escalation", "dropped").Inc()
		return
	}
	c.wg.Add(1)
	go func() {
		defer func() { <-c.inFlight; c.wg.Done() }()
		if err := c.Escalate(context.Background(), inc); err != nil {
			klog.V(4).Infof("escalation: %q: %v", inc.Title, err) // each failure is already logged and counted
		}
	}()
}

// Wait blocks until every Send has finished (shutdown and tests).
func (c *Chain) Wait() {
	if c != nil {
		c.wg.Wait()
	}
}

// --- PagerDuty (Events API v2) ---

type PagerDutyClient struct {
	routingKey string
	client     *http.Client
}

func NewPagerDuty(routingKey string, hc *http.Client) *PagerDutyClient {
	return &PagerDutyClient{routingKey: routingKey, client: httpx.Client(hc, 10*time.Second)}
}

func (p *PagerDutyClient) Trigger(ctx context.Context, inc Incident) error {
	inc = inc.redacted()
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
	return postJSON(ctx, p.client, "https://events.pagerduty.com/v2/enqueue", payload, nil)
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

func NewOpsGenie(apiKey string, hc *http.Client) *OpsGenieClient {
	return &OpsGenieClient{apiKey: apiKey, client: httpx.Client(hc, 10*time.Second)}
}

func (o *OpsGenieClient) Create(ctx context.Context, inc Incident) error {
	inc = inc.redacted()
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
	return postJSON(ctx, o.client, "https://api.opsgenie.com/v2/alerts", payload, map[string]string{"Authorization": "GenieKey " + o.apiKey})
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

// Send delivers one message. Unlike smtp.SendMail it is bounded: the dial
// and the whole conversation stop at ctx's deadline (ISS-080).
func (e *EmailClient) Send(ctx context.Context, inc Incident) error {
	inc = inc.redacted()
	subject := fmt.Sprintf("[auto-agent][%s] %s", strings.ToUpper(string(inc.Severity)), inc.Title)
	body := fmt.Sprintf("Subject: %s\r\nFrom: %s\r\nTo: %s\r\nContent-Type: text/plain\r\n\r\n%s\n\nNamespace: %s\nWorkload: %s",
		subject, e.from, e.to, inc.Body, inc.Namespace, inc.Workload)
	var auth smtp.Auth
	if e.user != "" {
		auth = smtp.PlainAuth("", e.user, e.pass, e.host)
	}
	return e.deliver(ctx, auth, strings.Split(e.to, ","), []byte(body))
}

// deliver is smtp.SendMail over a connection that honours ctx: STARTTLS
// when offered, AUTH only when the server supports it.
func (e *EmailClient) deliver(ctx context.Context, auth smtp.Auth, to []string, msg []byte) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(e.host, e.port))
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			conn.Close()
			return err
		}
	}
	c, err := smtp.NewClient(conn, e.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: e.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server does not support AUTH")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(e.from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(strings.TrimSpace(rcpt)); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// --- Helpers ---

// redacted returns a copy whose free text has passed through redaction, so
// no channel can send a secret off the cluster (ISS-042, constraint 8).
func (inc Incident) redacted() Incident {
	inc.Title = redact.String(inc.Title)
	inc.Body = redact.String(inc.Body)
	return inc
}

func postJSON(ctx context.Context, client *http.Client, url string, payload interface{}, headers map[string]string) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if rerr != nil {
			return fmt.Errorf("status %d (body unreadable: %v)", resp.StatusCode, rerr)
		}
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

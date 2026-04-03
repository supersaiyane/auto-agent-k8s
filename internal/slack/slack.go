package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Client supports posting to a default webhook and per-channel overrides.
type Client struct {
	defaultHook string
	client      *http.Client
	mu          sync.RWMutex
	channels    map[string]string // channel name -> webhook URL
}

func New(hook string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	return &Client{
		defaultHook: hook,
		client: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
		channels: make(map[string]string),
	}
}

// RegisterChannel adds a per-channel webhook URL (e.g. "#prod-incidents" -> webhook).
func (c *Client) RegisterChannel(channel, webhookURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channels[channel] = webhookURL
	klog.Infof("slack: registered channel %s", channel)
}

// Post sends a message to the default webhook.
func (c *Client) Post(text string) error {
	return c.postTo(c.defaultHook, text)
}

// Postf is a convenience method for formatted messages.
func (c *Client) Postf(format string, args ...any) error {
	return c.Post(fmt.Sprintf(format, args...))
}

// PostToChannel sends a message to a specific channel webhook.
// Falls back to the default webhook if the channel isn't registered.
func (c *Client) PostToChannel(channel, text string) error {
	hook := c.resolveChannel(channel)
	return c.postTo(hook, text)
}

// PostBlocks sends a Block Kit message (for interactive buttons).
func (c *Client) PostBlocks(blocks []map[string]interface{}) error {
	return c.postBlocksTo(c.defaultHook, blocks)
}

// PostBlocksToChannel sends Block Kit to a specific channel.
func (c *Client) PostBlocksToChannel(channel string, blocks []map[string]interface{}) error {
	hook := c.resolveChannel(channel)
	return c.postBlocksTo(hook, blocks)
}

func (c *Client) resolveChannel(channel string) string {
	if channel == "" {
		return c.defaultHook
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if hook, ok := c.channels[channel]; ok {
		return hook
	}
	return c.defaultHook
}

func (c *Client) postTo(hook, text string) error {
	if hook == "" {
		klog.V(4).Infof("slack: no webhook, skipping: %s", truncate(text, 80))
		return nil
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}
	resp, err := c.client.Post(hook, "application/json", bytes.NewReader(body))
	if err != nil {
		klog.Warningf("slack: post failed: %v", err)
		return fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) postBlocksTo(hook string, blocks []map[string]interface{}) error {
	if hook == "" {
		return nil
	}
	payload := map[string]interface{}{
		"blocks": blocks,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack: marshal blocks: %w", err)
	}
	resp, err := c.client.Post(hook, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: post blocks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack: blocks status %d", resp.StatusCode)
	}
	return nil
}

// BuildIncidentBlocks creates a Block Kit message with action buttons.
func BuildIncidentBlocks(title, detail, ns, workload, incidentID string) []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "header",
			"text": map[string]string{"type": "plain_text", "text": title},
		},
		{
			"type": "section",
			"text": map[string]string{"type": "mrkdwn", "text": detail},
		},
		{
			"type": "context",
			"elements": []map[string]string{
				{"type": "mrkdwn", "text": fmt.Sprintf("Namespace: `%s` | Workload: `%s`", ns, workload)},
			},
		},
		{
			"type": "actions",
			"elements": []map[string]interface{}{
				{
					"type":      "button",
					"text":      map[string]string{"type": "plain_text", "text": "Approve Fix"},
					"style":     "primary",
					"action_id": "approve_fix",
					"value":     incidentID,
				},
				{
					"type":      "button",
					"text":      map[string]string{"type": "plain_text", "text": "Rollback"},
					"style":     "danger",
					"action_id": "rollback",
					"value":     incidentID,
				},
				{
					"type":      "button",
					"text":      map[string]string{"type": "plain_text", "text": "Silence 1h"},
					"action_id": "silence_1h",
					"value":     ns + "/" + workload,
				},
			},
		},
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

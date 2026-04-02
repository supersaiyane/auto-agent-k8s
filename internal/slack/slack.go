package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"
)

type Client struct {
	hook   string
	client *http.Client
}

func New(hook string, timeoutSec int) *Client {
	if timeoutSec <= 0 {
		timeoutSec = 5
	}
	return &Client{
		hook: hook,
		client: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
		},
	}
}

func (c *Client) Post(text string) error {
	if c.hook == "" {
		klog.V(4).Infof("slack: no webhook configured, skipping: %s", truncate(text, 80))
		return nil
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("slack: marshal: %w", err)
	}
	resp, err := c.client.Post(c.hook, "application/json", bytes.NewReader(body))
	if err != nil {
		klog.Warningf("slack: post failed: %v", err)
		return fmt.Errorf("slack: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		klog.Warningf("slack: non-200 response: %d", resp.StatusCode)
		return fmt.Errorf("slack: status %d", resp.StatusCode)
	}
	return nil
}

// Postf is a convenience method for formatted messages.
func (c *Client) Postf(format string, args ...any) error {
	return c.Post(fmt.Sprintf(format, args...))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

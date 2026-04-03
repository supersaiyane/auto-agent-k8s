package kube

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// SelfCheck verifies the agent's own dependencies are reachable.
// Runs periodically and alerts if something is broken.
func SelfCheck(ctx context.Context, deps *Deps) {
	var issues []string

	// Check Prometheus connectivity
	promURL := os.Getenv("PROMETHEUS_URL")
	if promURL != "" {
		if err := httpCheck(ctx, promURL+"/-/healthy"); err != nil {
			issues = append(issues, fmt.Sprintf("Prometheus unreachable (%s): %v", promURL, err))
		}
	}

	// Check Slack connectivity
	slackURL := os.Getenv("SLACK_WEBHOOK_URL")
	if slackURL != "" {
		// Don't POST to slack, just check DNS/TCP
		if err := httpCheck(ctx, slackURL); err != nil {
			// Slack webhooks return 400 for GET but connection succeeding is enough
			if err.Error() != "status 400" && err.Error() != "status 404" && err.Error() != "status 405" {
				issues = append(issues, fmt.Sprintf("Slack webhook unreachable: %v", err))
			}
		}
	}

	// Check Alertmanager connectivity
	amURL := os.Getenv("ALERTMANAGER_URL")
	if amURL != "" {
		if err := httpCheck(ctx, amURL+"/-/healthy"); err != nil {
			issues = append(issues, fmt.Sprintf("Alertmanager unreachable (%s): %v", amURL, err))
		}
	}

	if len(issues) > 0 {
		key := dedupKey("", "self", "SelfCheckFailed")
		if deps.Dedup.Check(key) {
			msg := "*Agent Self-Check Failed*\n"
			for _, issue := range issues {
				msg += fmt.Sprintf("- %s\n", issue)
				klog.Warningf("selfcheck: %s", issue)
			}
			deps.Slack.Post(msg)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Info, Severity: eventsvc.SevWarning,
				Reason: "SelfCheckFailed", Message: fmt.Sprintf("%d dependency issues detected", len(issues))})
			obs.HandlerErrorsTotal.WithLabelValues("selfcheck", "dependency").Inc()
		}
	}
}

func httpCheck(ctx context.Context, url string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

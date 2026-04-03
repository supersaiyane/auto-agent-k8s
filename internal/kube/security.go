package kube

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckSecurityIssues scans for cert expiry, RBAC errors, and LimitRange violations.
func CheckSecurityIssues(ctx context.Context, deps *Deps) {
	checkCertExpiry(ctx, deps)
	checkLimitRangeViolations(ctx, deps)
	checkAPIServerThrottling(ctx, deps)
}

// checkCertExpiry scans TLS secrets for certificates expiring within 30 days.
func checkCertExpiry(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		secrets, err := deps.Client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
			FieldSelector: "type=kubernetes.io/tls",
		})
		if err != nil {
			continue
		}
		for _, secret := range secrets.Items {
			certPEM, ok := secret.Data["tls.crt"]
			if !ok {
				continue
			}
			block, _ := pem.Decode(certPEM)
			if block == nil {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}

			daysUntilExpiry := time.Until(cert.NotAfter).Hours() / 24

			if daysUntilExpiry < 0 {
				// Already expired
				key := dedupKey(ns, secret.Name, "CertExpired")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*CertExpired* TLS secret `%s/%s` has EXPIRED\n", ns, secret.Name)
				msg += fmt.Sprintf("Subject: %s\nExpired: %s (%.0f days ago)\n",
					cert.Subject.CommonName, cert.NotAfter.Format("2006-01-02"), -daysUntilExpiry)
				msg += "_Action required_: renew the certificate immediately.\n"
				deps.Slack.Post(msg)
				fireAlert(ctx, deps, "CertExpired", ns, secret.Name, "", msg, "critical")
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
					Namespace: ns, Workload: secret.Name, Reason: "CertExpired",
					Message: fmt.Sprintf("Expired %.0f days ago (%s)", -daysUntilExpiry, cert.Subject.CommonName)})
				obs.IncidentsTotal.WithLabelValues("CertExpired", ns, secret.Name).Inc()

			} else if daysUntilExpiry < 30 {
				// Expiring soon
				key := dedupKey(ns, secret.Name, "CertExpiringSoon")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*CertExpiringSoon* TLS secret `%s/%s` expires in %.0f days\n", ns, secret.Name, daysUntilExpiry)
				msg += fmt.Sprintf("Subject: %s\nExpires: %s\n", cert.Subject.CommonName, cert.NotAfter.Format("2006-01-02"))
				msg += "_Action_: renew before expiry. Check cert-manager or manual renewal.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: secret.Name, Reason: "CertExpiringSoon",
					Message: fmt.Sprintf("Expires in %.0f days (%s)", daysUntilExpiry, cert.Subject.CommonName)})
				obs.IncidentsTotal.WithLabelValues("CertExpiringSoon", ns, secret.Name).Inc()
			}
		}
	}
}

// checkLimitRangeViolations detects pods that violate namespace LimitRange defaults.
func checkLimitRangeViolations(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		lrs, err := deps.Client.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
		if err != nil || len(lrs.Items) == 0 {
			continue
		}
		// Just check if events mention LimitRange failures
		events, _ := deps.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if events == nil {
			continue
		}
		for _, ev := range events.Items {
			if ev.Reason == "FailedCreate" && (containsAny(ev.Message, "LimitRange", "forbidden", "exceeds") ||
				containsAny(ev.Message, "minimum", "maximum", "limit")) {
				if time.Since(ev.LastTimestamp.Time) > 10*time.Minute {
					continue
				}
				key := dedupKey(ns, ev.InvolvedObject.Name, "LimitRangeViolation")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*LimitRangeViolation* in `%s` for `%s/%s`\n",
					ns, ev.InvolvedObject.Kind, ev.InvolvedObject.Name)
				msg += fmt.Sprintf("Message: %s\n", ev.Message)
				msg += "_Fix_: adjust pod resource requests/limits to comply with LimitRange.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: ev.InvolvedObject.Name, Reason: "LimitRangeViolation",
					Message: ev.Message})
				obs.IncidentsTotal.WithLabelValues("LimitRangeViolation", ns, ev.InvolvedObject.Name).Inc()
			}
		}
	}
}

// checkAPIServerThrottling detects if the API server is throttling requests.
func checkAPIServerThrottling(ctx context.Context, deps *Deps) {
	// Check for 429 in recent events across all namespaces
	events, err := deps.Client.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: "reason=TooManyRequests",
		Limit:         10,
	})
	if err != nil || events == nil || len(events.Items) == 0 {
		return
	}
	for _, ev := range events.Items {
		if time.Since(ev.LastTimestamp.Time) > 10*time.Minute {
			continue
		}
		key := dedupKey("", "apiserver", "APIThrottled")
		if !deps.Dedup.Check(key) {
			return
		}
		msg := "*APIServerThrottled* — K8s API server is returning 429 Too Many Requests\n"
		msg += fmt.Sprintf("Source: %s — %s\n", ev.InvolvedObject.Name, ev.Message)
		msg += "_Check_: reduce API call frequency, check for controller loops.\n"
		deps.Slack.Post(msg)
		fireAlert(ctx, deps, "APIThrottled", "", "apiserver", "", msg, "warning")
		recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
			Reason: "APIThrottled", Message: "API server throttling requests"})
		obs.IncidentsTotal.WithLabelValues("APIThrottled", "", "apiserver").Inc()
		return
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// detectWebhookBlocking checks events for admission webhook rejections.
func CheckWebhookBlocking(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		events, err := deps.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ev := range events.Items {
			if ev.Reason != "FailedCreate" && ev.Reason != "FailedUpdate" {
				continue
			}
			if !containsAny(ev.Message, "webhook", "admission", "denied") {
				continue
			}
			if time.Since(ev.LastTimestamp.Time) > 10*time.Minute {
				continue
			}
			key := dedupKey(ns, ev.InvolvedObject.Name, "WebhookBlocking")
			if !deps.Dedup.Check(key) {
				continue
			}
			msg := fmt.Sprintf("*WebhookBlocking* in `%s` — admission webhook denied `%s/%s`\n",
				ns, ev.InvolvedObject.Kind, ev.InvolvedObject.Name)
			msg += fmt.Sprintf("Message: %s\n", ev.Message)
			msg += "_Check_: webhook configuration, or contact the webhook owner.\n"
			deps.Slack.Post(msg)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
				Namespace: ns, Workload: ev.InvolvedObject.Name, Reason: "WebhookBlocking",
				Message: ev.Message})
			obs.IncidentsTotal.WithLabelValues("WebhookBlocking", ns, ev.InvolvedObject.Name).Inc()
		}
	}
}

// CheckRBACDenied detects RBAC permission errors in events.
func CheckRBACDenied(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		events, err := deps.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ev := range events.Items {
			if !containsAny(ev.Message, "forbidden", "RBAC", "cannot") {
				continue
			}
			if time.Since(ev.LastTimestamp.Time) > 10*time.Minute {
				continue
			}
			key := dedupKey(ns, ev.InvolvedObject.Name, "RBACDenied")
			if !deps.Dedup.Check(key) {
				continue
			}
			klog.V(3).Infof("security: RBAC denied in %s: %s", ns, ev.Message)
			msg := fmt.Sprintf("*RBACDenied* in `%s` for `%s/%s`\n", ns, ev.InvolvedObject.Kind, ev.InvolvedObject.Name)
			msg += fmt.Sprintf("Message: %s\n", ev.Message)
			msg += "_Check_: ServiceAccount permissions, Role/RoleBinding.\n"
			deps.Slack.Post(msg)
			recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
				Namespace: ns, Workload: ev.InvolvedObject.Name, Reason: "RBACDenied", Message: ev.Message})
			obs.IncidentsTotal.WithLabelValues("RBACDenied", ns, ev.InvolvedObject.Name).Inc()
		}
	}
}

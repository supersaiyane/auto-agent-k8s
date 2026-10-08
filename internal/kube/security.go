package kube

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// eventLookback is how recent an event must be to be reported.
const eventLookback = 10 * time.Minute

// CheckSecurityIssues reports TLS certificates expired or expiring, and pods
// refused by a LimitRange. The phase 12 audit removed an API throttling
// check that read events in every namespace (constraint 4) for an event
// reason, TooManyRequests, that no Kubernetes component emits.
func CheckSecurityIssues(ctx context.Context, deps *Deps) {
	checkCertExpiry(ctx, deps)
	checkLimitRangeViolations(ctx, deps)
}

// certWarnDays is how early an expiring certificate is reported.
const certWarnDays = 30

// checkCertExpiry scans TLS secrets for certificates expired or expiring
// within certWarnDays, reminding once a day.
func checkCertExpiry(ctx context.Context, deps *Deps) {
	// Reading secrets is an opt-in grant (chart rbac.readTLSSecrets sets
	// TLS_CERT_CHECK); without it the check does not run at all (ISS-009).
	// The grant sits in the write Roles, so only the fix ceiling is read.
	if !deps.TLSCertCheck {
		return
	}
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		if !deps.Policy().InCeiling(ns) {
			continue
		}
		secrets, err := deps.Client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
		if err != nil {
			countAPIError(err, "secrets", ns)
			continue
		}
		for i := range secrets.Items {
			if f, ok := certFinding(&secrets.Items[i], now); ok && deps.Dedup.CheckFor(dedupKey(ns, secrets.Items[i].Name, f.Reason+"Daily"), 24*time.Hour) {
				report(ctx, deps, f)
			}
		}
	}
}

// certFinding reads the certificate in a TLS secret and says whether it is
// expired or expires within certWarnDays. Unreadable data is not a finding.
func certFinding(s *corev1.Secret, now time.Time) (finding, bool) {
	block, _ := pem.Decode(s.Data["tls.crt"])
	if block == nil {
		return finding{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return finding{}, false
	}
	days := cert.NotAfter.Sub(now).Hours() / 24
	f := finding{Namespace: s.Namespace, Workload: "secret/" + s.Name, Rung: RungGuided,
		Details: []string{fmt.Sprintf("Subject %s, not after %s", cert.Subject.CommonName, cert.NotAfter.UTC().Format("2006-01-02"))},
		Fix:     fmt.Sprintf("renew it; with cert-manager: `cmctl renew -n %s <certificate>`", s.Namespace)}
	switch {
	case days < 0:
		f.Reason, f.Severity, f.Summary = "CertExpired", eventsvc.SevCritical, fmt.Sprintf("the certificate expired %.0f days ago", -days)
	case days < certWarnDays:
		f.Reason, f.Severity, f.Summary = "CertExpiringSoon", eventsvc.SevWarning, fmt.Sprintf("the certificate expires in %.0f days", days)
	default:
		return finding{}, false
	}
	return f, true
}

// limitRangeRefusal matches the LimitRange admission plugin's wording
// ("minimum cpu usage per Container is 100m"); the audit found the check
// matched any "forbidden" or "limit", which quota, webhook and RBAC
// refusals also contain.
var limitRangeRefusal = regexp.MustCompile(`(?i)(minimum|maximum) \S+ usage per (Container|Pod)|LimitRange`)

// checkLimitRangeViolations reports recent pod creations a LimitRange refused.
func checkLimitRangeViolations(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		lrs, err := deps.Client.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "limitranges", ns) // phase 12: was dropped
			continue
		}
		if len(lrs.Items) == 0 {
			continue
		}
		for _, ev := range recentEvents(ctx, deps, ns) {
			if ev.Reason != "FailedCreate" || !limitRangeRefusal.MatchString(ev.Message) {
				continue
			}
			report(ctx, deps, eventFinding(&ev, "LimitRangeViolation", "a LimitRange refused the pod",
				"set requests and limits within the namespace's LimitRange (`kubectl describe limitrange -n "+ns+"`)"))
		}
	}
}

// recentEvents lists the events in ns from the last eventLookback; a failed
// read is counted and gives none.
func recentEvents(ctx context.Context, deps *Deps, ns string) []corev1.Event {
	list, err := deps.Client.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "events", ns)
		return nil
	}
	now := deps.clock()
	var out []corev1.Event
	for i := range list.Items {
		if now.Sub(eventTime(&list.Items[i])) <= eventLookback {
			out = append(out, list.Items[i])
		}
	}
	return out
}

// eventFinding builds a finding about the object an event names (R1).
func eventFinding(ev *corev1.Event, reason, summary, fix string) finding {
	return finding{Reason: reason, Namespace: ev.Namespace,
		Workload: strings.ToLower(ev.InvolvedObject.Kind) + "/" + ev.InvolvedObject.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Summary: summary, Details: []string{ev.Message}, Fix: fix}
}

// webhookRefusal matches an admission webhook that denied a request or could
// not be reached, and captures its name; the audit found any "denied" or
// "admission" matched, and an unreachable webhook was missed.
var webhookRefusal = regexp.MustCompile(`admission webhook "([^"]+)" denied|failed calling webhook "([^"]+)"`)

// CheckWebhookBlocking reports recent creates and updates an admission
// webhook refused or failed to answer.
func CheckWebhookBlocking(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		for _, ev := range recentEvents(ctx, deps, ns) {
			if ev.Reason != "FailedCreate" && ev.Reason != "FailedUpdate" {
				continue
			}
			m := webhookRefusal.FindStringSubmatch(ev.Message)
			if m == nil {
				continue
			}
			summary, fix := fmt.Sprintf("admission webhook `%s` denied the request", m[1]), "read the webhook's message; change the object or ask the webhook's owner"
			if m[2] != "" {
				summary = fmt.Sprintf("admission webhook `%s` could not be reached", m[2])
				fix = "the webhook's Service has no ready pod or its certificate is wrong; with failurePolicy Fail every matching request is refused until it answers"
			}
			report(ctx, deps, eventFinding(&ev, "WebhookBlocking", summary, fix))
		}
	}
}

// rbacRefusal matches the API server's RBAC denial: `User "x" cannot list
// resource "pods"`. The audit found any "forbidden", "RBAC" or "cannot"
// matched, which quota refusals and messages such as "cannot allocate
// memory" also contain.
var rbacRefusal = regexp.MustCompile(`cannot [a-z]+ resource "([^"]+)"`)

// CheckRBACDenied reports recent events where a controller or workload was
// refused by RBAC.
func CheckRBACDenied(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		for _, ev := range recentEvents(ctx, deps, ns) {
			m := rbacRefusal.FindStringSubmatch(ev.Message)
			if m == nil {
				continue
			}
			report(ctx, deps, eventFinding(&ev, "RBACDenied", fmt.Sprintf("refused by RBAC on `%s`", m[1]),
				"grant the ServiceAccount the verb on that resource with a Role and RoleBinding, or stop the call"))
		}
	}
}

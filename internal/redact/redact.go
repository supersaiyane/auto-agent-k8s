// Package redact masks secrets and personal data in text before it leaves the
// cluster: LLM prompts, Slack messages, tickets, pull request text and
// Alertmanager annotations (CLAUDE.md constraint 8, ISS-011).
//
// It is applied inside the clients that do the network call, so no caller can
// forget it. Log bundles written to the operator's own storage are not
// redacted: they are the forensic record.
package redact

import "regexp"

type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules run in order. Earlier rules remove whole structures (key blocks, URL
// credentials) so later, narrower rules do not see half of them.
var rules = []rule{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[redacted private key]"},
	// scheme://user:password@host keeps the host. "***" is outside the email
	// rule's character class, so the host is not then taken for an email.
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}***@"},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]+=*`), "${1}[redacted]"},
	{regexp.MustCompile(`(?i)("?\b(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|secret[_-]?key|client[_-]?secret|private[_-]?key)"?\s*[:=]\s*"?)[^\s"',;&}]+`), "${1}[redacted]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[redacted aws key]"},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), "[redacted github token]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[redacted slack token]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), "[redacted api key]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), "[redacted jwt]"},
	{regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), "[redacted email]"},
	// The address goes, a following :port stays: it is usually the diagnosis.
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "[ipv4]"},
}

// String returns s with secrets and personal data masked.
func String(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// Map returns a copy of m with every value redacted.
func Map(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = String(v)
	}
	return out
}

// Value returns a copy of v with every string inside it redacted. It walks the
// shapes JSON payloads are built from; other values are returned unchanged.
func Value(v any) any {
	switch t := v.(type) {
	case string:
		return String(t)
	case map[string]string:
		return Map(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = Value(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = Value(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(t))
		for i, e := range t {
			out[i] = Value(e).(map[string]any)
		}
		return out
	case []map[string]string:
		out := make([]map[string]string, len(t))
		for i, e := range t {
			out[i] = Map(e)
		}
		return out
	default:
		return v
	}
}

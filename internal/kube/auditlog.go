package kube

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// AuditEntry represents a single remediation action taken by the agent.
type AuditEntry struct {
	Timestamp time.Time         `json:"timestamp"`
	Action    string            `json:"action"`
	Namespace string            `json:"namespace"`
	Workload  string            `json:"workload"`
	Pod       string            `json:"pod,omitempty"`
	Node      string            `json:"node,omitempty"`
	Reason    string            `json:"reason"`
	Result    string            `json:"result"` // "success", "failed", "blocked", "skipped"
	Detail    string            `json:"detail,omitempty"`
	Mode      string            `json:"mode"` // fix, suggest, observe
	Extras    map[string]string `json:"extras,omitempty"`
}

// AuditLog provides persistent action logging that survives pod restarts.
type AuditLog struct {
	mu      sync.Mutex
	logFile *os.File
	encoder *json.Encoder
	path    string
}

// NewAuditLog creates an audit log that writes to the given path.
// If path is empty, logs to /var/log/auto-agent/audit.jsonl.
func NewAuditLog(path string) *AuditLog {
	if path == "" {
		path = "/var/log/auto-agent/audit.jsonl"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		klog.Warningf("audit: failed to create dir for %s: %v", path, err)
		return &AuditLog{path: path} // will retry on first write
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		klog.Warningf("audit: failed to open %s: %v", path, err)
		return &AuditLog{path: path}
	}
	return &AuditLog{
		logFile: f,
		encoder: json.NewEncoder(f),
		path:    path,
	}
}

// Record writes an audit entry to the persistent log.
func (a *AuditLog) Record(entry AuditEntry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// Lazy open on first write or after failure
	if a.logFile == nil {
		f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			klog.V(2).Infof("audit: cannot write: %v", err)
			return
		}
		a.logFile = f
		a.encoder = json.NewEncoder(f)
	}

	if err := a.encoder.Encode(entry); err != nil {
		klog.V(2).Infof("audit: encode failed: %v", err)
		// Close and retry next time
		a.logFile.Close()
		a.logFile = nil
	}
}

// Close closes the audit log file.
func (a *AuditLog) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logFile != nil {
		a.logFile.Close()
		a.logFile = nil
	}
}

// RecordAction is a convenience method for recording a remediation action.
func (a *AuditLog) RecordAction(action, ns, workload, pod, reason, result, detail, mode string) {
	a.Record(AuditEntry{
		Action:    action,
		Namespace: ns,
		Workload:  workload,
		Pod:       pod,
		Reason:    reason,
		Result:    result,
		Detail:    detail,
		Mode:      mode,
	})
}

// BlastRadiusTracker limits actions across namespaces within a time window.
type BlastRadiusTracker struct {
	mu              sync.Mutex
	namespaceCounts map[string]int
	window          time.Duration
	maxNamespaces   int
	lastReset       time.Time
}

func NewBlastRadiusTracker(maxNamespaces int, window time.Duration) *BlastRadiusTracker {
	return &BlastRadiusTracker{
		namespaceCounts: make(map[string]int),
		window:          window,
		maxNamespaces:   maxNamespaces,
		lastReset:       time.Now(),
	}
}

// AllowAction returns true if action is allowed in this namespace (not exceeding blast radius).
func (b *BlastRadiusTracker) AllowAction(ns string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Reset if window expired
	if time.Since(b.lastReset) >= b.window {
		b.namespaceCounts = make(map[string]int)
		b.lastReset = time.Now()
	}

	// Count distinct namespaces that have been acted on
	if _, exists := b.namespaceCounts[ns]; !exists {
		distinctCount := len(b.namespaceCounts)
		if distinctCount >= b.maxNamespaces {
			return false // blast radius limit reached
		}
	}

	b.namespaceCounts[ns]++
	return true
}

// AffectedNamespaces returns the count of namespaces with actions in the current window.
func (b *BlastRadiusTracker) AffectedNamespaces() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.namespaceCounts)
}

// QuietHours checks if the current time falls within a maintenance window.
type QuietHours struct {
	enabled bool
	windows []TimeWindow
}

type TimeWindow struct {
	Start string // "HH:MM" in UTC
	End   string // "HH:MM" in UTC
}

func NewQuietHours(windows string) *QuietHours {
	if windows == "" {
		return &QuietHours{enabled: false}
	}
	qh := &QuietHours{enabled: true}
	// Parse "02:00-06:00,14:00-14:30" format
	for _, w := range splitComma(windows) {
		parts := splitDash(w)
		if len(parts) == 2 {
			qh.windows = append(qh.windows, TimeWindow{Start: parts[0], End: parts[1]})
		}
	}
	return qh
}

// IsQuiet returns true if now is within a quiet window. Actions should be suppressed.
func (qh *QuietHours) IsQuiet() bool {
	if !qh.enabled || len(qh.windows) == 0 {
		return false
	}
	now := time.Now().UTC()
	nowStr := fmt.Sprintf("%02d:%02d", now.Hour(), now.Minute())
	for _, w := range qh.windows {
		if w.Start <= w.End {
			// Normal window: 02:00-06:00
			if nowStr >= w.Start && nowStr < w.End {
				return true
			}
		} else {
			// Overnight window: 22:00-06:00
			if nowStr >= w.Start || nowStr < w.End {
				return true
			}
		}
	}
	return false
}

func splitComma(s string) []string {
	parts := make([]string, 0)
	for _, p := range splitBy(s, ',') {
		p = trimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func splitDash(s string) []string {
	return splitBy(s, '-')
}

func splitBy(s string, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

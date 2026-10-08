package kube

import (
	"context"
	"net/http"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/supersaiyane/auto-agent-k8s/internal/alertmanager"
	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	"github.com/supersaiyane/auto-agent-k8s/internal/escalation"
	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
	"github.com/supersaiyane/auto-agent-k8s/internal/storage"
)

// SlackPoster abstracts Slack message posting for testability.
type SlackPoster interface {
	Post(text string) error
	Postf(format string, args ...any) error
}

// LLMDiagnoser abstracts LLM diagnosis for testability.
type LLMDiagnoser interface {
	Enabled() bool
	Diagnose(ctx context.Context, title, diagContext string) string
	DiagnoseWithFallback(ctx context.Context, title, diagContext string) string
}

// Deps holds shared dependencies injected into all kube handlers.
type Deps struct {
	Client kubernetes.Interface
	// NodeName is the node this agent pod runs on (downward API NODE_NAME).
	// Node actions are taken only for this node; empty means none (ISS-004).
	NodeName string
	// ScalingGates are optional PromQL gates for scale-up (PLAN-002 8.3).
	ScalingGates config.ScalingGates
	// TLSCertCheck turns on the certificate expiry check (rbac.readTLSSecrets).
	TLSCertCheck bool
	// Endpoints the self check probes; empty ones are skipped.
	Endpoints SelfCheckEndpoints
	Metrics   metrics.Provider
	// Policies supplies the current policy snapshot. Read it through
	// Deps.Policy(); never store a policy in a shared field (ISS-007).
	Policies PolicySource

	// Now is the clock detectors use for their time windows; nil means
	// time.Now (PLAN-002 phase 10).
	Now func() time.Time

	// HTTPClient is used for runbook and self-check calls; nil means a
	// default client (PLAN-002 9.2).
	HTTPClient *http.Client

	// handlerSlots bounds concurrent handlers; StartWatchers creates it.
	handlerSlots chan struct{}
	// inflight counts running handlers, so shutdown can wait for them
	// before closing the audit log (ISS-054). A pointer: Deps is copied.
	inflight      *sync.WaitGroup
	Slack         SlackPoster
	LLM           LLMDiagnoser
	Dedup         *ratelimit.Deduplicator
	Limiter       *ratelimit.ActionLimiter
	Sink          storage.Sink
	CRDStore      *crd.Store
	GitOps        integrations.GitOps
	Ticketer      integrations.Ticketer
	Recorder      events.Sink
	Breaker       *ratelimit.CircuitBreaker
	AlertManager  *alertmanager.Client
	AuditLog      *AuditLog
	BlastRadius   *BlastRadiusTracker
	QuietHours    *QuietHours
	DryRunLog     *DryRunLog
	FixTracker    *FixTracker
	Escalation    *escalation.Chain
	DeployTracker *DeployTracker
	LearningMode  *LearningMode
	Approvals     *Approvals // R3 approval queue; nil or no approvers: off
}

// PolicySource returns the current immutable policy snapshot. In production
// it is the ConfigMap hot reloader; tests use policy.Static.
type PolicySource interface {
	Get() *policy.Policy
}

// Policy returns the current policy snapshot. A reload replaces the snapshot
// rather than changing it, so a caller may hold the returned value.
func (d *Deps) Policy() *policy.Policy {
	return d.Policies.Get()
}

// SelfCheckEndpoints are the external endpoints SelfCheck probes.
type SelfCheckEndpoints struct {
	PrometheusURL   string
	SlackWebhookURL string
	AlertmanagerURL string
}

// clock returns the current time from Deps.Now, or time.Now.
func (d *Deps) clock() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// WaitIdle waits up to timeout for running handlers to finish and reports
// whether they did. Shutdown calls it after intake has stopped and before
// the audit log closes, so an action in flight is still recorded (ISS-054).
func (d *Deps) WaitIdle(timeout time.Duration) bool {
	if d.inflight == nil {
		return true
	}
	done := make(chan struct{})
	go func() { d.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

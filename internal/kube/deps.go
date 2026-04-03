package kube

import (
	"context"

	"k8s.io/client-go/kubernetes"

	"github.com/yourorg/auto-agent/internal/crd"
	"github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/integrations"
	"github.com/yourorg/auto-agent/internal/metrics"
	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
	"github.com/yourorg/auto-agent/internal/storage"
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
	Client   kubernetes.Interface
	Metrics  metrics.Provider
	Policy   *policy.Policy
	Slack    SlackPoster
	LLM      LLMDiagnoser
	Dedup    *ratelimit.Deduplicator
	Limiter  *ratelimit.ActionLimiter
	Sink     storage.Sink
	CRDStore *crd.Store
	GitOps   integrations.GitOps
	Ticketer integrations.Ticketer
	Recorder *events.Recorder
}

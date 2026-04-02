package kube

import (
	"k8s.io/client-go/kubernetes"

	"github.com/yourorg/auto-agent/internal/crd"
	"github.com/yourorg/auto-agent/internal/llm"
	"github.com/yourorg/auto-agent/internal/metrics"
	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
	"github.com/yourorg/auto-agent/internal/slack"
	"github.com/yourorg/auto-agent/internal/storage"
)

// Deps holds shared dependencies injected into all kube handlers.
type Deps struct {
	Client   *kubernetes.Clientset
	Metrics  metrics.Provider
	Policy   *policy.Policy
	Slack    *slack.Client
	LLM      *llm.Client
	Dedup    *ratelimit.Deduplicator
	Limiter  *ratelimit.ActionLimiter
	Sink     storage.Sink
	CRDStore *crd.Store
}

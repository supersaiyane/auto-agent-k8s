package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/crd"
	"github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/httpapi"
	"github.com/yourorg/auto-agent/internal/integrations"
	"github.com/yourorg/auto-agent/internal/kube"
	"github.com/yourorg/auto-agent/internal/leader"
	"github.com/yourorg/auto-agent/internal/llm"
	"github.com/yourorg/auto-agent/internal/metrics"
	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
	"github.com/yourorg/auto-agent/internal/ratelimit"
	"github.com/yourorg/auto-agent/internal/slack"
	"github.com/yourorg/auto-agent/internal/storage"
)

const version = "1.0.0"

func main() {
	klog.InitFlags(nil)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Load policy ---
	pol := policy.LoadFromEnv()
	klog.Infof("auto-agent %s starting (mode=%s, namespaces=%v)", version, pol.Mode, namespaceList(pol))

	// --- Publish info metric ---
	obs.InfoGauge.WithLabelValues(version, string(pol.Mode)).Set(1)

	// --- Event recorder for UI dashboard ---
	recorder := events.NewRecorder(500)

	// --- HTTP server (health + metrics + dashboard UI) ---
	httpSrv := httpapi.NewServer(":8080", recorder, &httpapi.AgentMeta{
		Version:  version,
		Mode:     string(pol.Mode),
		NodeName: os.Getenv("NODE_NAME"),
		PodName:  os.Getenv("POD_NAME"),
	})
	go httpSrv.Start()

	// --- Kubernetes clients ---
	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("in-cluster config: %v", err)
	}
	// Increase QPS for high-throughput remediation
	cfg.QPS = 50
	cfg.Burst = 100

	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("kube client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("dynamic client: %v", err)
	}

	// --- Initialize dependencies ---
	sl := slack.New(os.Getenv("SLACK_WEBHOOK_URL"), pol.SlackTimeoutSec)
	ll := llm.New(
		os.Getenv("LLM_API_URL"),
		os.Getenv("LLM_API_KEY"),
		os.Getenv("LLM_MODEL"),
		pol.LLMEnabled,
		pol.LLMTimeoutSec,
	)

	mp, err := metrics.NewProviderFromEnv(ctx)
	if err != nil {
		klog.Fatalf("metrics provider: %v", err)
	}

	sink := storage.GlobalSink()
	dedup := ratelimit.NewDeduplicator(time.Duration(pol.DedupTTLSeconds) * time.Second)
	limiter := ratelimit.NewActionLimiter(pol.MaxActionsPer10m, 10*time.Minute)

	// CRD store + controller
	crdStore := crd.NewStore()
	crd.StartController(ctx, dyn, crdStore)

	// --- GitOps client ---
	var gitOps integrations.GitOps
	gitToken := os.Getenv("GIT_TOKEN")
	gitRepo := os.Getenv("GITOPS_REPO")
	gitBranch := os.Getenv("GITOPS_BRANCH")
	if gitToken != "" && gitRepo != "" {
		switch os.Getenv("GITOPS_PROVIDER") {
		case "gitlab":
			gitOps = integrations.NewGitLab(gitToken, gitRepo, gitBranch)
		default:
			gitOps = integrations.NewGitHub(gitToken, gitRepo, gitBranch)
		}
		klog.Infof("gitops: configured (%s)", os.Getenv("GITOPS_PROVIDER"))
	}

	// --- Ticketing client ---
	var ticketer integrations.Ticketer
	if os.Getenv("TICKETS_ENABLED") == "true" {
		switch os.Getenv("TICKETS_PROVIDER") {
		case "jira":
			ticketer = integrations.NewJira(
				os.Getenv("JIRA_TOKEN"),
				os.Getenv("JIRA_BASE_URL"),
				os.Getenv("JIRA_PROJECT_KEY"),
				os.Getenv("JIRA_EMAIL"),
			)
			klog.Infof("tickets: configured (jira)")
		case "github":
			ticketer = integrations.NewGitHubIssues(os.Getenv("GITHUB_TOKEN"), os.Getenv("GITHUB_REPO"))
			klog.Infof("tickets: configured (github)")
		default:
			ticketer = integrations.NewNopTicketer()
		}
	}

	// --- Build dependency struct ---
	deps := &kube.Deps{
		Client:   kc,
		Metrics:  mp,
		Policy:   pol,
		Slack:    sl,
		LLM:     ll,
		Dedup:    dedup,
		Limiter:  limiter,
		Sink:     sink,
		CRDStore: crdStore,
		GitOps:   gitOps,
		Ticketer: ticketer,
		Recorder: recorder,
	}

	// --- Leader election (for cluster-wide scaling) ---
	le := leader.Start(ctx, kc, "auto-agent-leader")
	httpSrv.SetLeaderFunc(le.IsLeader)

	// --- Start watchers (pod + node informers) ---
	kube.StartWatchers(ctx, deps)

	// --- Mark ready ---
	httpSrv.SetReady()
	sl.Postf("auto-agent %s started on node `%s` (mode=%s)", version, hostname(), pol.Mode)

	// --- Log retention cleanup (filesystem only) ---
	go kube.StartLogRetention(ctx)

	// --- Leader-only periodic loops ---
	go func() {
		scaleTicker := time.NewTicker(30 * time.Second)
		jobTicker := time.NewTicker(2 * time.Minute)
		quotaTicker := time.NewTicker(5 * time.Minute)
		defer scaleTicker.Stop()
		defer jobTicker.Stop()
		defer quotaTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-scaleTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.EvaluateAndScale(ctx, deps)
				kube.CheckAnomalies(ctx, deps)
			case <-jobTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckFailedJobs(ctx, deps)
			case <-quotaTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckResourceQuotas(ctx, deps)
			}
		}
	}()

	// --- Wait for shutdown ---
	<-ctx.Done()
	klog.Infof("shutting down...")

	// Graceful shutdown with 10s deadline
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	dedup.Stop()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		klog.Warningf("http shutdown: %v", err)
	}
	sl.Post("auto-agent shutting down")
	klog.Infof("auto-agent stopped")
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func namespaceList(pol *policy.Policy) []string {
	nss := make([]string, 0, len(pol.NamespaceAllow))
	for ns := range pol.NamespaceAllow {
		nss = append(nss, ns)
	}
	return nss
}

package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/alertmanager"
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
	"github.com/yourorg/auto-agent/internal/webhook"
)

const version = "1.0.0"

func main() {
	klog.InitFlags(nil)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Kubernetes clients ---
	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("in-cluster config: %v", err)
	}
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

	// --- Load policy (with ConfigMap hot-reload) ---
	podNS := os.Getenv("POD_NAMESPACE")
	if podNS == "" {
		podNS = "kube-system"
	}
	pol := policy.LoadFromEnv()
	hotReloader := policy.NewHotReloader(pol, podNS, "auto-agent-config")
	go hotReloader.Start(ctx, kc)

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

	// --- Admission webhook (optional, requires TLS certs) ---
	webhookCert := os.Getenv("WEBHOOK_CERT_FILE")
	webhookKey := os.Getenv("WEBHOOK_KEY_FILE")
	if webhookCert != "" && webhookKey != "" {
		blockedImages := strings.Split(os.Getenv("WEBHOOK_BLOCKED_IMAGES"), ",")
		wh := webhook.NewValidator(webhook.Config{
			Port:             8443,
			RequireLimits:    os.Getenv("WEBHOOK_REQUIRE_LIMITS") != "false",
			RequireReadiness: os.Getenv("WEBHOOK_REQUIRE_READINESS") != "false",
			BlockedImages:    blockedImages,
		})
		go wh.Start(webhookCert, webhookKey)
		klog.Infof("webhook: admission validator enabled on :8443")
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
	breaker := ratelimit.NewCircuitBreaker(5, 1*time.Hour)

	// Alertmanager client (optional)
	am := alertmanager.New(os.Getenv("ALERTMANAGER_URL"))

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

	// --- Audit log (persistent, survives restarts) ---
	auditLog := kube.NewAuditLog(os.Getenv("AUDIT_LOG_PATH"))

	// --- Blast radius tracker (max namespaces affected per hour) ---
	blastRadius := kube.NewBlastRadiusTracker(5, 1*time.Hour) // max 5 namespaces per hour

	// --- Quiet hours / maintenance windows ---
	quietHours := kube.NewQuietHours(os.Getenv("QUIET_HOURS")) // e.g. "02:00-06:00"

	// --- Build dependency struct ---
	deps := &kube.Deps{
		Client:       kc,
		Metrics:      mp,
		Policy:       pol,
		Slack:        sl,
		LLM:          ll,
		Dedup:        dedup,
		Limiter:      limiter,
		Sink:         sink,
		CRDStore:     crdStore,
		GitOps:       gitOps,
		Ticketer:     ticketer,
		Recorder:     recorder,
		Breaker:      breaker,
		AlertManager: am,
		AuditLog:     auditLog,
		BlastRadius:  blastRadius,
		QuietHours:   quietHours,
	}

	// --- Leader election (for cluster-wide scaling) ---
	le := leader.Start(ctx, kc, "auto-agent-leader")
	httpSrv.SetLeaderFunc(le.IsLeader)

	// --- Start watchers (pod + node informers) ---
	kube.StartWatchers(ctx, deps)

	// --- Log retention cleanup (filesystem only) ---
	go kube.StartLogRetention(ctx)

	// --- Mark ready ---
	httpSrv.SetReady()
	sl.Postf("auto-agent %s started on node `%s` (mode=%s)", version, hostname(), pol.Mode)

	// --- Leader-only periodic loops ---
	go func() {
		scaleTicker := time.NewTicker(30 * time.Second)
		jobTicker := time.NewTicker(2 * time.Minute)
		quotaTicker := time.NewTicker(5 * time.Minute)
		healthTicker := time.NewTicker(3 * time.Minute)
		defer scaleTicker.Stop()
		defer jobTicker.Stop()
		defer quotaTicker.Stop()
		defer healthTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-scaleTicker.C:
				if !le.IsLeader() {
					continue
				}
				deps.Policy = hotReloader.Get()
				kube.EvaluateAndScale(ctx, deps)
				kube.CheckAnomalies(ctx, deps)
			case <-jobTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckFailedJobs(ctx, deps)
				kube.CheckStuckRollouts(ctx, deps)
				kube.CleanupEvictedPods(ctx, deps)
				kube.CheckServiceEndpoints(ctx, deps)
			case <-quotaTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckResourceQuotas(ctx, deps)
			case <-healthTicker.C:
				kube.SelfCheck(ctx, deps)
			}
		}
	}()

	// --- Wait for shutdown ---
	<-ctx.Done()
	klog.Infof("shutting down...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	dedup.Stop()
	auditLog.Close()
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

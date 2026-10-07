package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/alertmanager"
	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	"github.com/supersaiyane/auto-agent-k8s/internal/escalation"
	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpapi"
	"github.com/supersaiyane/auto-agent-k8s/internal/integrations"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
	"github.com/supersaiyane/auto-agent-k8s/internal/leader"
	"github.com/supersaiyane/auto-agent-k8s/internal/llm"
	"github.com/supersaiyane/auto-agent-k8s/internal/logging"
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
	"github.com/supersaiyane/auto-agent-k8s/internal/slack"
	"github.com/supersaiyane/auto-agent-k8s/internal/storage"
	"github.com/supersaiyane/auto-agent-k8s/internal/webhook"
)

// version is set at build time: -ldflags "-X main.version=<v>" (ISS-018).
var version = "dev"

func main() {
	klog.InitFlags(nil)
	logging.Init() // structured JSON if LOG_FORMAT=json

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
	recorder.EnablePersistence("/var/log/auto-agent/events.jsonl")

	// --- HTTP server (health + metrics + dashboard UI) ---
	httpSrv := httpapi.NewServer(":8080", recorder, &httpapi.AgentMeta{
		Version:  version,
		Mode:     string(pol.Mode),
		NodeName: os.Getenv("NODE_NAME"),
		PodName:  os.Getenv("POD_NAME"),
	}, kc)
	httpSrv.SetNamespaceFilter(func(ns string) bool { return hotReloader.Get().AllowedNamespace(ns) })
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
	breaker := ratelimit.NewCircuitBreaker(intEnv("CIRCUIT_BREAKER_THRESHOLD", 5), 1*time.Hour)

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
	blastRadius := kube.NewBlastRadiusTracker(intEnv("BLAST_RADIUS_MAX_NAMESPACES", 5), 1*time.Hour) // distinct namespaces acted on per hour

	// --- Quiet hours / maintenance windows ---
	quietHours := kube.NewQuietHours(os.Getenv("QUIET_HOURS")) // e.g. "02:00-06:00"

	// --- Escalation chain (PagerDuty, OpsGenie, email) ---
	escChain := escalation.NewChain()

	// --- Deploy tracker (incident correlation) ---
	deployTracker := kube.NewDeployTracker(100)

	// --- Compliance tracker ---
	complianceTracker := kube.NewComplianceTracker()

	// --- Learning mode (baseline collection) ---
	var learningMode *kube.LearningMode
	if os.Getenv("LEARNING_ENABLED") == "true" {
		days := 14
		if v := os.Getenv("LEARNING_PERIOD_DAYS"); v != "" {
			fmt.Sscanf(v, "%d", &days)
		}
		learningMode = kube.NewLearningMode("", time.Duration(days)*24*time.Hour)
		klog.Infof("learning: enabled (period=%d days)", days)
	}

	// --- Dry-run log ---
	// Always created: the mode can switch to dry-run by ConfigMap reload after
	// startup, and SimulateAction records nothing without a log (ISS-013).
	dryRunLog := kube.NewDryRunLog(200)
	if pol.Mode == policy.DryRun {
		klog.Infof("dry-run: mode enabled, no actions will be taken, simulations logged")
	}

	// --- Fix tracker (verifies actions actually fixed the problem) ---
	fixTracker := kube.NewFixTracker(200)

	// Wire extended API deps
	httpapi.SetExtendedDeps(complianceTracker, learningMode, deployTracker, dryRunLog, fixTracker)

	// --- Build dependency struct ---
	deps := &kube.Deps{
		Client:        kc,
		NodeName:      os.Getenv("NODE_NAME"),
		Metrics:       mp,
		Policies:      hotReloader,
		Slack:         sl,
		LLM:           ll,
		Dedup:         dedup,
		Limiter:       limiter,
		Sink:          sink,
		CRDStore:      crdStore,
		GitOps:        gitOps,
		Ticketer:      ticketer,
		Recorder:      recorder,
		Breaker:       breaker,
		AlertManager:  am,
		AuditLog:      auditLog,
		BlastRadius:   blastRadius,
		QuietHours:    quietHours,
		DryRunLog:     dryRunLog,
		Escalation:    escChain,
		DeployTracker: deployTracker,
		LearningMode:  learningMode,
		Compliance:    complianceTracker,
		FixTracker:    fixTracker,
	}

	// --- Leader election (for cluster-wide scaling) ---
	le := leader.Start(ctx, kc, envOr("LEADER_LEASE_NAMESPACE", "kube-system"), "auto-agent-leader")
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
		// Intervals are configurable (ISS-015); the e2e shortens them so every
		// detector runs within one test.
		scaleTicker := time.NewTicker(durationEnv("SCALE_INTERVAL", 30*time.Second))
		jobTicker := time.NewTicker(durationEnv("JOB_INTERVAL", 2*time.Minute))
		quotaTicker := time.NewTicker(durationEnv("QUOTA_INTERVAL", 5*time.Minute))
		healthTicker := time.NewTicker(durationEnv("HEALTH_INTERVAL", 3*time.Minute))
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
				kube.EvaluateAndScale(ctx, deps)
				kube.CheckAnomalies(ctx, deps)
				kube.VerifyFixes(ctx, deps)
			case <-jobTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckFailedJobs(ctx, deps)
				kube.CheckStuckRollouts(ctx, deps)
				kube.CleanupEvictedPods(ctx, deps)
				kube.CheckServiceEndpoints(ctx, deps)
				kube.CheckPendingPVCs(ctx, deps)
				kube.CheckNodeHealth(ctx, deps)
				kube.CheckNodeExtended(ctx, deps)
				kube.ScanDeployments(ctx, deps)
				kube.CheckDeadlineExceeded(ctx, deps)
				kube.CheckEphemeralStorageFull(ctx, deps)
				kube.CheckStatefulSetStuck(ctx, deps)
				kube.CheckDaemonSetMissing(ctx, deps)
				kube.CheckHPAIssues(ctx, deps)
				kube.CheckCronJobMissed(ctx, deps)
				kube.CheckDeploymentPaused(ctx, deps)
				kube.CheckReplicaSetFailure(ctx, deps)
			case <-quotaTicker.C:
				if !le.IsLeader() {
					continue
				}
				kube.CheckResourceQuotas(ctx, deps)
				kube.CollectBaselines(ctx, deps)
				kube.CheckStorageIssues(ctx, deps)
				kube.CheckVolumeAttachments(ctx, deps)
				kube.CheckNetworkIssues(ctx, deps)
				kube.CheckSecurityIssues(ctx, deps)
				kube.CheckWebhookBlocking(ctx, deps)
				kube.CheckRBACDenied(ctx, deps)
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
	recorder.Close()
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

// durationEnv reads a positive duration such as "45s" from the environment,
// falling back to def when unset or invalid.
func durationEnv(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		klog.Warningf("invalid %s %q, using %s", name, v, def)
		return def
	}
	return d
}

// intEnv reads a positive integer from the environment, falling back to def
// when unset or invalid (ISS-015).
func intEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		klog.Warningf("invalid %s %q, using %d", name, v, def)
		return def
	}
	return n
}

// envOr returns the environment value of name, or def when it is unset.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

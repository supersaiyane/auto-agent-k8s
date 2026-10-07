package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/alertmanager"
	"github.com/supersaiyane/auto-agent-k8s/internal/config"
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
	// The only environment read in the agent (PLAN-002 8.3).
	conf := config.Load(os.Getenv)
	if v, ok := config.KlogVerbosity(conf.Policy.LogLevel); ok {
		_ = flag.Set("v", strconv.Itoa(v)) // agent.logLevel (ISS-032)
	} else {
		klog.Warningf("unknown LOG_LEVEL %q, keeping klog defaults", conf.Policy.LogLevel)
	}
	logging.Init(conf.LogFormat)

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
	podNS := conf.PodNamespace
	if podNS == "" {
		podNS = "kube-system"
	}
	pol := conf.Policy
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
		NodeName: conf.NodeName,
		PodName:  conf.PodName,
	}, kc, httpapi.Options{DashboardToken: conf.DashboardToken, SlackSigningSecret: conf.SlackSigningSecret})
	httpSrv.SetNamespaceFilter(func(ns string) bool { return hotReloader.Get().AllowedNamespace(ns) })
	go httpSrv.Start()

	// --- Admission webhook (optional, requires TLS certs) ---
	if conf.Webhook.Enabled() {
		// An unset list is empty, not [""]; an empty prefix would block every
		// image (ISS-041).
		wh := webhook.NewValidator(webhook.Config{
			Port:             8443,
			RequireLimits:    conf.Webhook.RequireLimits,
			RequireReadiness: conf.Webhook.RequireReadiness,
			BlockedImages:    conf.Webhook.BlockedImages,
		})
		go wh.Start(conf.Webhook.CertFile, conf.Webhook.KeyFile)
		klog.Infof("webhook: admission validator enabled on :8443")
	}

	// --- Initialize dependencies ---
	sl := slack.New(conf.SlackWebhookURL, pol.SlackTimeoutSec)
	ll := llm.New(
		conf.LLMAPIURL,
		conf.LLMAPIKey,
		conf.LLMModel,
		pol.LLMEnabled,
		pol.LLMTimeoutSec,
	)

	mp, err := metrics.NewProvider(conf.MetricsProvider, conf.PrometheusURL)
	if err != nil {
		klog.Fatalf("metrics provider: %v", err)
	}

	sink := storage.NewSink(conf.Storage)
	dedup := ratelimit.NewDeduplicator(time.Duration(pol.DedupTTLSeconds) * time.Second)
	limiter := ratelimit.NewActionLimiter(pol.MaxActionsPer10m, 10*time.Minute)
	breaker := ratelimit.NewCircuitBreaker(conf.CircuitBreakerThreshold, 1*time.Hour)

	// Alertmanager client (optional)
	am := alertmanager.New(conf.AlertmanagerURL)

	// CRD store + controller
	crdStore := crd.NewStore()
	crd.StartController(ctx, dyn, crdStore)

	// --- GitOps client ---
	var gitOps integrations.GitOps
	gitToken, gitRepo, gitBranch := conf.GitToken, conf.GitOpsRepo, conf.GitOpsBranch
	if gitToken != "" && gitRepo != "" {
		switch conf.GitOpsProvider {
		case "gitlab":
			gitOps = integrations.NewGitLab(gitToken, gitRepo, gitBranch)
		default:
			gitOps = integrations.NewGitHub(gitToken, gitRepo, gitBranch)
		}
		klog.Infof("gitops: configured (%s)", conf.GitOpsProvider)
	}

	// --- Ticketing client ---
	var ticketer integrations.Ticketer
	if conf.TicketsEnabled {
		switch conf.TicketsProvider {
		case "jira":
			ticketer = integrations.NewJira(
				conf.JiraToken,
				conf.JiraBaseURL,
				conf.JiraProjectKey,
				conf.JiraEmail,
			)
			klog.Infof("tickets: configured (jira)")
		case "github":
			ticketer = integrations.NewGitHubIssues(conf.GitHubToken, conf.GitHubRepo)
			klog.Infof("tickets: configured (github)")
		default:
			ticketer = integrations.NewNopTicketer()
		}
	}

	// --- Audit log (persistent, survives restarts) ---
	auditLog := kube.NewAuditLog(conf.AuditLogPath)

	// --- Blast radius tracker (max namespaces affected per hour) ---
	blastRadius := kube.NewBlastRadiusTracker(conf.BlastRadiusMaxNamespaces, 1*time.Hour) // distinct namespaces acted on per hour

	// --- Quiet hours / maintenance windows ---
	quietHours := kube.NewQuietHours(conf.QuietHours) // e.g. "02:00-06:00"

	// --- Escalation chain (PagerDuty, OpsGenie, email) ---
	escChain := escalation.NewChain(conf.Escalation)

	// --- Deploy tracker (incident correlation) ---
	deployTracker := kube.NewDeployTracker(100)

	// --- Compliance tracker ---
	complianceTracker := kube.NewComplianceTracker()

	// --- Learning mode (baseline collection) ---
	var learningMode *kube.LearningMode
	if conf.LearningEnabled {
		days := conf.LearningPeriodDays
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
		Client:       kc,
		NodeName:     conf.NodeName,
		ScalingGates: conf.ScalingGates,
		TLSCertCheck: conf.TLSCertCheck,
		Endpoints: kube.SelfCheckEndpoints{
			PrometheusURL:   conf.PrometheusURL,
			SlackWebhookURL: conf.SlackWebhookURL,
			AlertmanagerURL: conf.AlertmanagerURL,
		},
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
	le := leader.Start(ctx, kc, conf.LeaderLeaseNamespace, "auto-agent-leader", conf.PodName)
	httpSrv.SetLeaderFunc(le.IsLeader)

	// --- Start watchers (pod + node informers) ---
	kube.StartWatchers(ctx, deps)

	// --- Log retention cleanup (filesystem only) ---
	go kube.StartLogRetention(ctx, conf.Storage, conf.LogRetentionDays)

	// --- Mark ready ---
	httpSrv.SetReady()
	sl.Postf("auto-agent %s started on node `%s` (mode=%s)", version, hostname(), pol.Mode)

	// --- Leader-only periodic loops ---
	go func() {
		// Intervals are configurable (ISS-015); the e2e shortens them so every
		// detector runs within one test.
		scaleTicker := time.NewTicker(conf.ScaleInterval)
		jobTicker := time.NewTicker(conf.JobInterval)
		quotaTicker := time.NewTicker(conf.QuotaInterval)
		healthTicker := time.NewTicker(conf.HealthInterval)
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

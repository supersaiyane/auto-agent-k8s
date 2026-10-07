package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/ratelimit"
	"github.com/supersaiyane/auto-agent-k8s/internal/slack"
	"github.com/supersaiyane/auto-agent-k8s/internal/storage"
	"github.com/supersaiyane/auto-agent-k8s/internal/webhook"
)

// Clients are what run talks to: main passes real clients, tests pass fakes.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// HTTP is used for every outbound call (Slack, LLM, tickets, PRs,
	// Alertmanager, escalation, Prometheus); nil means default clients.
	HTTP *http.Client
}

// RunOptions are process-level settings a test overrides.
type RunOptions struct {
	HTTPAddr   string // dashboard and probes; default ":8080"
	EventsPath string // event persistence; default under /var/log/auto-agent
	OnReady    func() // called once the agent is ready (tests)
	// ForwardEvery is how often a node agent sends its events; default 5s.
	ForwardEvery time.Duration
}

// roles says what this process runs (ADR-001).
type roles struct{ node, controller bool }

func rolesFor(role string) (roles, error) {
	switch role {
	case config.RoleAll:
		return roles{node: true, controller: true}, nil
	case config.RoleNode:
		return roles{node: true}, nil
	case config.RoleController:
		return roles{controller: true}, nil
	}
	return roles{}, fmt.Errorf("AGENT_ROLE %q: want %s, %s or %s", role, config.RoleAll, config.RoleNode, config.RoleController)
}

// run wires and runs the agent until ctx is cancelled (PLAN-002 9.5).
// Every dependency is built before the HTTP server starts, so no handler
// sees a half-initialised dependency.
func run(ctx context.Context, conf config.Config, cl Clients, opts RunOptions) error {
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = ":8080"
	}
	if opts.EventsPath == "" {
		opts.EventsPath = config.DefaultLogDir + "/events.jsonl"
	}
	if opts.ForwardEvery == 0 {
		opts.ForwardEvery = 5 * time.Second
	}
	rl, err := rolesFor(conf.Role)
	if err != nil {
		return err
	}
	onlyNode := rl.node && !rl.controller
	if onlyNode && (conf.ControllerURL == "" || conf.InternalToken == "") {
		return fmt.Errorf("AGENT_ROLE=node needs CONTROLLER_URL and INTERNAL_TOKEN")
	}

	// --- Policy with ConfigMap hot reload ---
	podNS := conf.PodNamespace
	if podNS == "" {
		podNS = "kube-system"
	}
	pol := conf.Policy
	hotReloader := policy.NewHotReloader(pol, podNS, "auto-agent-config")
	go hotReloader.Start(ctx, cl.Kube)

	klog.Infof("auto-agent %s starting (role=%s, mode=%s, namespaces=%v)", version, conf.Role, pol.Mode, namespaceList(pol))
	obs.InfoGauge.WithLabelValues(version, string(pol.Mode)).Set(1)

	// The controller keeps the one event log; a node agent forwards to it.
	var recorder *events.Recorder
	var sink events.Sink
	forwarded := make(chan struct{})
	if onlyNode {
		fwd := events.NewForwarder(conf.ControllerURL, conf.InternalToken, conf.NodeName, cl.HTTP)
		go func() { fwd.Run(ctx, opts.ForwardEvery); close(forwarded) }()
		sink = fwd
	} else {
		close(forwarded)
		recorder = events.NewRecorder(500)
		recorder.EnablePersistence(opts.EventsPath)
		sink = recorder
	}

	// --- Admission webhook (optional, requires TLS certs; controller) ---
	if rl.controller && conf.Webhook.Enabled() {
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

	// --- Outbound clients ---
	sl := slack.New(conf.SlackWebhookURL, pol.SlackTimeoutSec, cl.HTTP)
	ll := llm.New(conf.LLMAPIURL, conf.LLMAPIKey, conf.LLMModel, pol.LLMEnabled, pol.LLMTimeoutSec, cl.HTTP)
	mp, err := metrics.NewProvider(conf.MetricsProvider, conf.PrometheusURL, cl.HTTP)
	if err != nil {
		return fmt.Errorf("metrics provider: %w", err)
	}
	am := alertmanager.New(conf.AlertmanagerURL, cl.HTTP)
	gitOps := newGitOps(conf, cl.HTTP)
	ticketer := newTicketer(conf, cl.HTTP)
	escChain := escalation.NewChain(conf.Escalation, cl.HTTP)

	// --- Guardrails and trackers ---
	dedup := ratelimit.NewDeduplicator(time.Duration(pol.DedupTTLSeconds) * time.Second)
	limiter := ratelimit.NewActionLimiter(pol.MaxActionsPer10m, 10*time.Minute)
	breaker := ratelimit.NewCircuitBreaker(conf.CircuitBreakerThreshold, 1*time.Hour)
	auditLog := kube.NewAuditLog(conf.AuditLogPath)
	blastRadius := kube.NewBlastRadiusTracker(conf.BlastRadiusMaxNamespaces, 1*time.Hour)
	quietHours := kube.NewQuietHours(conf.QuietHours)
	deployTracker := kube.NewDeployTracker(100)
	complianceTracker := kube.NewComplianceTracker()
	var learningMode *kube.LearningMode
	if conf.LearningEnabled {
		learningMode = kube.NewLearningMode("", time.Duration(conf.LearningPeriodDays)*24*time.Hour)
		klog.Infof("learning: enabled (period=%d days)", conf.LearningPeriodDays)
	}
	// Always created: the mode can switch to dry-run by ConfigMap reload after
	// startup, and SimulateAction records nothing without a log (ISS-013).
	dryRunLog := kube.NewDryRunLog(200)
	if pol.Mode == policy.DryRun {
		klog.Infof("dry-run: mode enabled, no actions will be taken, simulations logged")
	}
	fixTracker := kube.NewFixTracker(200)

	crdStore := crd.NewStore()
	crd.StartController(ctx, cl.Dynamic, crdStore)

	deps := &kube.Deps{
		Client:       cl.Kube,
		NodeName:     conf.NodeName,
		ScalingGates: conf.ScalingGates,
		TLSCertCheck: conf.TLSCertCheck,
		Endpoints: kube.SelfCheckEndpoints{
			PrometheusURL:   conf.PrometheusURL,
			SlackWebhookURL: conf.SlackWebhookURL,
			AlertmanagerURL: conf.AlertmanagerURL,
		},
		HTTPClient:    cl.HTTP,
		Metrics:       mp,
		Policies:      hotReloader,
		Slack:         sl,
		LLM:           ll,
		Dedup:         dedup,
		Limiter:       limiter,
		Sink:          storage.NewSink(conf.Storage),
		CRDStore:      crdStore,
		GitOps:        gitOps,
		Ticketer:      ticketer,
		Recorder:      sink,
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

	// --- Leader election (cluster-wide loops; controller only) ---
	var le *leader.Elector
	isLeader := func() bool { return false }
	if rl.controller {
		le = leader.Start(ctx, cl.Kube, conf.LeaderLeaseNamespace, "auto-agent-leader", conf.PodName)
		isLeader = le.IsLeader
	}

	// --- HTTP server: built last, with everything it serves ---
	httpSrv := httpapi.NewServer(opts.HTTPAddr, recorder, &httpapi.AgentMeta{
		Version:  version,
		Mode:     string(pol.Mode),
		NodeName: conf.NodeName,
		PodName:  conf.PodName,
	}, cl.Kube, httpapi.Options{
		DashboardToken:     conf.DashboardToken,
		SlackSigningSecret: conf.SlackSigningSecret,
		Cost:               conf.Cost,
		HTTPClient:         cl.HTTP,
		AllowNamespace:     func(ns string) bool { return hotReloader.Get().AllowedNamespace(ns) },
		IsLeader:           isLeader,
		HealthOnly:         onlyNode,
		InternalToken:      conf.InternalToken,
		Extended: httpapi.ExtendedDeps{
			Compliance: complianceTracker,
			Learning:   learningMode,
			Deploys:    deployTracker,
			DryRun:     dryRunLog,
			Fixes:      fixTracker,
		},
	})
	go httpSrv.Start()

	if rl.node {
		kube.StartWatchers(ctx, deps)
		go kube.StartLogRetention(ctx, conf.Storage, conf.LogRetentionDays)
	}

	httpSrv.SetReady()
	if opts.OnReady != nil {
		opts.OnReady()
	}
	if rl.controller { // node agents stay quiet: one notice per rollout, not per node (ISS-055)
		if err := sl.Postf("auto-agent %s started on `%s` (mode=%s)", version, hostname(), pol.Mode); err != nil {
			klog.V(2).Infof("slack: start message not sent: %v", err)
		}
		go leaderLoops(ctx, conf, deps, le)
	}

	<-ctx.Done()
	klog.Infof("shutting down...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	dedup.Stop()
	<-forwarded // the node agent's last drain to the controller
	if recorder != nil {
		recorder.Close()
	}
	auditLog.Close()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		klog.Warningf("http shutdown: %v", err)
	}
	if rl.controller {
		if err := sl.Post("auto-agent shutting down"); err != nil {
			klog.V(2).Infof("slack: stop message not sent: %v", err)
		}
	}
	klog.Infof("auto-agent stopped")
	return nil
}

// leaderLoops runs the periodic checks; all but the self check run only on
// the leader. Intervals are configurable (ISS-015).
func leaderLoops(ctx context.Context, conf config.Config, deps *kube.Deps, le *leader.Elector) {
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
			kube.CheckPodStates(ctx, deps)
		case <-quotaTicker.C:
			if !le.IsLeader() {
				continue
			}
			kube.CheckResourceQuotas(ctx, deps)
			kube.CollectBaselines(ctx, deps)
			kube.CheckStorageIssues(ctx, deps)
			kube.CheckNetworkIssues(ctx, deps)
			kube.CheckSecurityIssues(ctx, deps)
			kube.CheckWebhookBlocking(ctx, deps)
			kube.CheckRBACDenied(ctx, deps)
			kube.CheckStuckFinalizers(ctx, deps)
			kube.CheckDisruptionBudgets(ctx, deps)
			kube.CheckResourcePressure(ctx, deps)
			kube.CheckControlPlane(ctx, deps)
		case <-healthTicker.C:
			kube.SelfCheck(ctx, deps)
		}
	}
}

// newGitOps returns the pull request client, or nil when not configured.
func newGitOps(conf config.Config, hc *http.Client) integrations.GitOps {
	if conf.GitToken == "" || conf.GitOpsRepo == "" {
		return nil
	}
	klog.Infof("gitops: configured (%s)", conf.GitOpsProvider)
	if conf.GitOpsProvider == "gitlab" {
		return integrations.NewGitLab(conf.GitToken, conf.GitOpsRepo, conf.GitOpsBranch, hc)
	}
	return integrations.NewGitHub(conf.GitToken, conf.GitOpsRepo, conf.GitOpsBranch, hc)
}

// newTicketer returns the ticket client, or nil when tickets are off.
func newTicketer(conf config.Config, hc *http.Client) integrations.Ticketer {
	if !conf.TicketsEnabled {
		return nil
	}
	switch conf.TicketsProvider {
	case "jira":
		klog.Infof("tickets: configured (jira)")
		return integrations.NewJira(conf.JiraToken, conf.JiraBaseURL, conf.JiraProjectKey, conf.JiraEmail, hc)
	case "github":
		klog.Infof("tickets: configured (github)")
		return integrations.NewGitHubIssues(conf.GitHubToken, conf.GitHubRepo, hc)
	default:
		return integrations.NewNopTicketer()
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func namespaceList(pol *policy.Policy) []string {
	nss := make([]string, 0, len(pol.NamespaceAllow))
	for ns := range pol.NamespaceAllow {
		nss = append(nss, ns)
	}
	return nss
}

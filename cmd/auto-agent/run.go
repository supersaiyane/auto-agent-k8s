package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Shutdown budget: the pod's termination grace period (30s) must cover all
// of it (ISS-054).
const (
	httpGrace    = 5 * time.Second  // in-flight API requests
	handlerGrace = 15 * time.Second // pod handlers and leader loops
	drainGrace   = 5 * time.Second  // the last event forward
)

// agent is one running process: what run builds and shutdown takes down.
type agent struct {
	conf      config.Config
	rl        roles
	hr        *policy.HotReloader
	podNS     string
	ev        eventPipe
	deps      *kube.Deps
	extras    trackers
	le        *leader.Elector
	leads     func() bool // le.IsLeader on controllers; tests swap it
	srv       *httpapi.Server
	loopsDone chan struct{}
	reloader  *kube.Reloader // config reload on controllers (PLAN-002 Part A); nil when off
	leading   atomic.Bool    // this process announced leadership and still leads
}

// run wires and runs the agent until ctx is cancelled (PLAN-002 9.5).
// Every dependency is built before the HTTP server starts, so no handler
// sees a half-initialised dependency.
func run(ctx context.Context, conf config.Config, cl Clients, opts RunOptions) error {
	opts = withDefaults(opts)
	rl, err := rolesFor(conf.Role)
	if err != nil {
		return err
	}
	if rl.onlyNode() && (conf.ControllerURL == "" || conf.InternalToken == "") {
		return fmt.Errorf("AGENT_ROLE=node needs CONTROLLER_URL and INTERNAL_TOKEN")
	}
	a := &agent{conf: conf, rl: rl, loopsDone: make(chan struct{}), leads: func() bool { return false }}
	a.startPolicy(ctx, cl.Kube)
	// Forwarders outlive ctx: they stop only after every handler has
	// recorded its last event (ISS-054).
	fwdCtx, stopForwarding := context.WithCancel(context.Background())
	defer stopForwarding()
	a.ev = startEvents(ctx, fwdCtx, conf, cl, rl, opts, a.isLeader)
	startWebhook(conf, rl)
	if err := a.buildDeps(ctx, cl); err != nil {
		return err
	}
	leaderTarget := a.startLeader(ctx, cl.Kube, opts)
	if rl.controller && conf.Reload.Enabled {
		a.reloader = kube.NewReloader(a.deps, conf.Reload, a.isLeader)
	}
	a.srv = a.newServer(cl, opts, leaderTarget)
	go a.srv.Start()
	a.startWork(ctx)
	if opts.OnReady != nil {
		opts.OnReady()
	}
	<-ctx.Done()
	a.shutdown(stopForwarding)
	return nil
}

func withDefaults(opts RunOptions) RunOptions {
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = ":8080"
	}
	if opts.EventsPath == "" {
		opts.EventsPath = config.DefaultLogDir + "/events.jsonl"
	}
	if opts.ForwardEvery == 0 {
		opts.ForwardEvery = 5 * time.Second
	}
	return opts
}

func (r roles) onlyNode() bool { return r.node && !r.controller }

// startPolicy starts the ConfigMap hot reload; both roles apply the
// dashboard's fix scope (ADR-002).
func (a *agent) startPolicy(ctx context.Context, kc kubernetes.Interface) {
	a.podNS = a.conf.PodNamespace
	if a.podNS == "" {
		a.podNS = "kube-system"
	}
	pol := a.conf.Policy
	a.hr = policy.NewHotReloader(pol, a.podNS, "auto-agent-config")
	a.hr.WatchScope(policy.ScopeConfigMap)
	go a.hr.Start(ctx, kc)
	klog.Infof("auto-agent %s starting (role=%s, mode=%s, %s)", version, a.conf.Role, pol.Mode, scopeSummary(pol))
	obs.InfoGauge.WithLabelValues(version, string(pol.Mode)).Set(1)
}

// eventPipe is where events go: the controller keeps the one log, a node
// agent forwards to it (ADR-001). drained closes after the last forward.
type eventPipe struct {
	recorder *events.Recorder
	sink     events.Sink
	drained  chan struct{}
}

func startEvents(ctx, fwdCtx context.Context, conf config.Config, cl Clients, rl roles, opts RunOptions, leading func() bool) eventPipe {
	ev := eventPipe{drained: make(chan struct{})}
	if rl.onlyNode() {
		fwd := events.NewForwarder(conf.ControllerURL, conf.InternalToken, conf.NodeName, cl.HTTP)
		go func() { fwd.Run(fwdCtx, opts.ForwardEvery); close(ev.drained) }()
		ev.sink = fwd
		return ev
	}
	ev.recorder = events.NewRecorder(500)
	ev.recorder.EnablePersistence(opts.EventsPath)
	ev.sink = ev.recorder
	if !rl.controller || rl.node {
		close(ev.drained)
		return ev
	}
	// The leader copies its log to the standby, and a starting controller
	// copies a peer's log first, so a leader change keeps the history (ISS-059).
	peers := newPeerResolver(cl.Kube, conf.PodNamespace, conf.PodName, httpPort(opts.HTTPAddr))
	bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
	if n, err := events.Backfill(bctx, peers, conf.InternalToken, cl.HTTP, ev.recorder); err != nil {
		klog.Warningf("events: history not copied from a peer: %v", err)
	} else if n > 0 {
		klog.Infof("events: copied %d events from a peer controller", n)
	}
	bcancel()
	replica := events.NewReplicaForwarder(peers, conf.InternalToken, cl.HTTP)
	go func() { replica.Run(fwdCtx, opts.ForwardEvery); close(ev.drained) }()
	ev.sink = events.Tee{Local: ev.recorder, Copy: replica, Leading: leading}
	return ev
}

// startWebhook starts the admission validator (optional, needs TLS certs;
// controller only).
func startWebhook(conf config.Config, rl roles) {
	if !rl.controller || !conf.Webhook.Enabled() {
		return
	}
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

// trackers are the stateful parts the server shows and shutdown closes.
type trackers struct {
	slack    *slack.Client
	dedup    *ratelimit.Deduplicator
	audit    *kube.AuditLog
	learning *kube.LearningMode
	deploys  *kube.DeployTracker
	dryRun   *kube.DryRunLog
	fixes    *kube.FixTracker
}

func newTrackers(conf config.Config, cl Clients) trackers {
	pol := conf.Policy
	t := trackers{
		slack:   slack.New(conf.SlackWebhookURL, pol.SlackTimeoutSec, cl.HTTP),
		dedup:   ratelimit.NewDeduplicator(time.Duration(pol.DedupTTLSeconds) * time.Second),
		audit:   kube.NewAuditLog(conf.AuditLogPath),
		deploys: kube.NewDeployTracker(100),
		// Always created: the mode can switch to dry-run by reload after
		// start, and SimulateAction records nothing without a log (ISS-013).
		dryRun: kube.NewDryRunLog(200),
		fixes:  kube.NewFixTracker(200),
	}
	if conf.LearningEnabled {
		t.learning = kube.NewLearningMode("", time.Duration(conf.LearningPeriodDays)*24*time.Hour)
		klog.Infof("learning: enabled (period=%d days)", conf.LearningPeriodDays)
	}
	if pol.Mode == policy.DryRun {
		klog.Infof("dry-run: mode enabled, no actions will be taken, simulations logged")
	}
	return t
}

// buildDeps builds everything handlers and loops use.
func (a *agent) buildDeps(ctx context.Context, cl Clients) error {
	conf, pol := a.conf, a.conf.Policy
	mp, err := metrics.NewProvider(conf.MetricsProvider, conf.PrometheusURL, cl.HTTP)
	if err != nil {
		return fmt.Errorf("metrics provider: %w", err)
	}
	a.extras = newTrackers(conf, cl)
	crdStore := crd.NewStore()
	crd.StartController(ctx, cl.Dynamic, crdStore)
	t := a.extras
	a.deps = &kube.Deps{
		Client: cl.Kube, NodeName: conf.NodeName, ScalingGates: conf.ScalingGates, TLSCertCheck: conf.TLSCertCheck,
		Endpoints: kube.SelfCheckEndpoints{PrometheusURL: conf.PrometheusURL, SlackWebhookURL: conf.SlackWebhookURL,
			AlertmanagerURL: conf.AlertmanagerURL},
		HTTPClient: cl.HTTP, Metrics: mp, Policies: a.hr, Slack: t.slack,
		LLM:   llm.New(conf.LLMAPIURL, conf.LLMAPIKey, conf.LLMModel, pol.LLMEnabled, pol.LLMTimeoutSec, cl.HTTP),
		Dedup: t.dedup, Limiter: ratelimit.NewActionLimiter(pol.MaxActionsPer10m, 10*time.Minute),
		Sink: storage.NewSink(conf.Storage), CRDStore: crdStore,
		GitOps: newGitOps(conf, cl.HTTP), Ticketer: newTicketer(conf, cl.HTTP), Recorder: a.ev.sink,
		Breaker:      ratelimit.NewCircuitBreaker(conf.CircuitBreakerThreshold, time.Hour),
		AlertManager: alertmanager.New(conf.AlertmanagerURL, cl.HTTP), AuditLog: t.audit,
		BlastRadius: kube.NewBlastRadiusTracker(conf.BlastRadiusMaxNamespaces, time.Hour),
		QuietHours:  kube.NewQuietHours(conf.QuietHours), DryRunLog: t.dryRun,
		Escalation: escalation.NewChain(conf.Escalation, cl.HTTP), DeployTracker: t.deploys,
		LearningMode: t.learning, FixTracker: t.fixes,
	}
	return nil
}

// startLeader starts leader election on controllers and returns where a
// standby proxies to (nil: never proxy, ADR-001).
func (a *agent) startLeader(ctx context.Context, kc kubernetes.Interface, opts RunOptions) func() (string, error) {
	if !a.rl.controller {
		return nil
	}
	a.le = leader.Start(ctx, kc, a.conf.LeaderLeaseNamespace, "auto-agent-leader", a.conf.PodName)
	a.leads = a.le.IsLeader
	if a.rl.node {
		return nil
	}
	return newLeaderTarget(kc, a.conf.PodNamespace, httpPort(opts.HTTPAddr), a.le)
}

func (a *agent) isLeader() bool { return a.leads() }

// newServer builds the HTTP server last, with everything it serves.
func (a *agent) newServer(cl Clients, opts RunOptions, leaderTarget func() (string, error)) *httpapi.Server {
	var scope httpapi.ScopeOptions
	if a.rl.controller {
		scope = httpapi.ScopeOptions{Policy: a.hr.Get, Save: func(ctx context.Context, names []string, from string) error {
			return kube.SaveFixScope(ctx, a.deps, a.podNS, names, from)
		}}
	}
	t := a.extras
	ext := httpapi.ExtendedDeps{Learning: t.learning, Deploys: t.deploys, DryRun: t.dryRun, Fixes: t.fixes}
	if a.reloader != nil { // a typed nil would look set
		ext.Reloads = a.reloader
	}
	return httpapi.NewServer(opts.HTTPAddr, a.ev.recorder, &httpapi.AgentMeta{
		Version: version, Mode: string(a.conf.Policy.Mode), NodeName: a.conf.NodeName, PodName: a.conf.PodName,
	}, cl.Kube, httpapi.Options{
		DashboardToken: a.conf.DashboardToken, SlackSigningSecret: a.conf.SlackSigningSecret,
		Cost: a.conf.Cost, HTTPClient: cl.HTTP,
		AllowNamespace: func(ns string) bool { return a.hr.Get().Watched(ns) },
		IsLeader:       a.isLeader, Leader: leaderTarget, HealthOnly: a.rl.onlyNode(),
		Ingest: a.ev.sink, InternalToken: a.conf.InternalToken, Scope: scope,
		Extended: ext,
	})
}

// startWork starts the watchers (node role) and the leader loops and the
// leadership notice (controller role), then marks the server ready.
func (a *agent) startWork(ctx context.Context) {
	if a.rl.node {
		kube.StartWatchers(ctx, a.deps)
		go kube.StartLogRetention(ctx, a.conf.Storage, a.conf.LogRetentionDays)
	}
	a.srv.SetReady()
	if !a.rl.controller {
		close(a.loopsDone)
		return
	}
	if a.reloader != nil {
		a.reloader.WatchConfig(ctx)
	}
	go func() { leaderLoops(ctx, a.conf, a.deps, a.le); close(a.loopsDone) }()
	go a.announceLeadership(ctx, 2*time.Second)
}

// announceLeadership posts one start notice when this controller becomes
// leader, so a rollout sends one per leader term instead of one per pod
// (ISS-055); shutdown sends the stop notice only while this process leads.
func (a *agent) announceLeadership(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		leads := a.isLeader()
		if leads && !a.leading.Load() {
			if err := a.extras.slack.Postf("auto-agent %s leading on `%s` (mode=%s)", version, hostname(), a.hr.Get().Mode); err != nil {
				klog.V(2).Infof("slack: start message not sent: %v", err)
			}
		}
		a.leading.Store(leads)
	}
}

// shutdown stops intake, waits (bounded) for handlers and loops to finish,
// then for the last event forward, and only then closes the event log and
// the audit log, so an action in flight at SIGTERM is still recorded (ISS-054).
func (a *agent) shutdown(stopForwarding context.CancelFunc) {
	klog.Infof("shutting down...")
	hctx, cancel := context.WithTimeout(context.Background(), httpGrace)
	defer cancel()
	if err := a.srv.Shutdown(hctx); err != nil {
		klog.Warningf("http shutdown: %v", err)
	}
	if !a.deps.WaitIdle(handlerGrace) {
		klog.Warningf("shutdown: handlers still running after %s", handlerGrace)
	}
	if !waitClosed(a.loopsDone, handlerGrace) {
		klog.Warningf("shutdown: leader loops still running after %s", handlerGrace)
	}
	a.extras.dedup.Stop()
	stopForwarding()
	if !waitClosed(a.ev.drained, drainGrace) {
		klog.Warningf("shutdown: last event forward did not finish in %s", drainGrace)
	}
	if a.ev.recorder != nil {
		a.ev.recorder.Close()
	}
	a.extras.audit.Close()
	if a.leading.Load() {
		if err := a.extras.slack.Post("auto-agent shutting down: the standby takes over"); err != nil {
			klog.V(2).Infof("slack: stop message not sent: %v", err)
		}
	}
	klog.Infof("auto-agent stopped")
}

// waitClosed waits up to d for ch to close and reports whether it did.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// check is one periodic detector or loop.
type check func(context.Context, *kube.Deps)

// checkGroup runs its checks every interval; leaderOnly groups are skipped
// on a standby. Intervals are configurable (ISS-015).
type checkGroup struct {
	every      time.Duration
	leaderOnly bool
	checks     []check
}

func leaderChecks(conf config.Config) []checkGroup {
	return []checkGroup{
		{conf.ScaleInterval, true, []check{kube.EvaluateAndScale, kube.CheckAnomalies, kube.VerifyFixes}},
		{conf.JobInterval, true, []check{kube.CheckFailedJobs, kube.CheckStuckRollouts, kube.CleanupEvictedPods,
			kube.CheckServiceEndpoints, kube.CheckPendingPVCs, kube.CheckNodeHealth, kube.CheckNodeExtended,
			kube.ScanDeployments, kube.CheckDeadlineExceeded, kube.CheckEphemeralStorageFull, kube.CheckStatefulSetStuck,
			kube.CheckDaemonSetMissing, kube.CheckHPAIssues, kube.CheckCronJobMissed, kube.CheckDeploymentPaused,
			kube.CheckReplicaSetFailure, kube.CheckPodStates}},
		{conf.QuotaInterval, true, []check{kube.CheckResourceQuotas, kube.CollectBaselines, kube.CheckStorageIssues,
			kube.CheckNetworkIssues, kube.CheckSecurityIssues, kube.CheckWebhookBlocking, kube.CheckRBACDenied,
			kube.CheckStuckFinalizers, kube.CheckDisruptionBudgets, kube.CheckResourcePressure, kube.CheckControlPlane}},
		{conf.HealthInterval, false, []check{kube.SelfCheck}},
	}
}

// leaderLoops runs the check groups on their intervals, one group at a time,
// until ctx is cancelled; all but the self check run only on the leader.
func leaderLoops(ctx context.Context, conf config.Config, deps *kube.Deps, le *leader.Elector) {
	groups := leaderChecks(conf)
	due := make(chan int)
	for i, g := range groups {
		go tick(ctx, g.every, i, due)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case i := <-due:
			if groups[i].leaderOnly && !le.IsLeader() {
				continue
			}
			for _, c := range groups[i].checks {
				c(ctx, deps)
			}
		}
	}
}

// tick sends i on due every interval until ctx is cancelled.
func tick(ctx context.Context, every time.Duration, i int, due chan<- int) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			select {
			case due <- i:
			case <-ctx.Done():
				return
			}
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

// scopeSummary describes the watch scope and fix scope for the start log.
func scopeSummary(pol *policy.Policy) string {
	watch := "all non-system namespaces"
	if names, all := pol.WatchList(); !all {
		watch = strings.Join(names, ",")
	}
	return fmt.Sprintf("watch=%s fix=%s anywhere=%v", watch, strings.Join(pol.FixScope(), ","), pol.FixAnywhere)
}

// electorView is what the leader resolver needs from the elector.
type electorView interface {
	IsLeader() bool
	Leader() string
}

// newLeaderTarget resolves the leader's base URL from its identity (its
// pod name) and pod IP, caching the last answer by identity.
func newLeaderTarget(kc kubernetes.Interface, ns, port string, el electorView) func() (string, error) {
	var mu sync.Mutex
	var cachedID, cachedURL string
	return func() (string, error) {
		if el.IsLeader() {
			return "", nil
		}
		id := el.Leader()
		if id == "" {
			return "", fmt.Errorf("leader not elected yet")
		}
		mu.Lock()
		defer mu.Unlock()
		if id == cachedID {
			return cachedURL, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p, err := kc.CoreV1().Pods(ns).Get(ctx, id, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("leader pod %s/%s: %w", ns, id, err)
		}
		if p.Status.PodIP == "" {
			return "", fmt.Errorf("leader pod %s/%s has no IP yet", ns, id)
		}
		cachedID, cachedURL = id, "http://"+net.JoinHostPort(p.Status.PodIP, port)
		return cachedURL, nil
	}
}

// httpPort is the port part of a listen address such as ":8080".
func httpPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "8080"
}

// newPeerResolver lists the other running controllers in this namespace, the
// standbys the leader copies its event log to (ISS-059).
func newPeerResolver(kc kubernetes.Interface, ns, self, port string) func() ([]string, error) {
	return func() ([]string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pods, err := kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: controllerSelector})
		if err != nil {
			return nil, fmt.Errorf("list controllers in %s: %w", ns, err)
		}
		var peers []string
		for _, p := range pods.Items {
			if p.Name == self || p.Status.PodIP == "" || p.Status.Phase != "Running" {
				continue
			}
			peers = append(peers, "http://"+net.JoinHostPort(p.Status.PodIP, port))
		}
		return peers, nil
	}
}

// controllerSelector matches the controller pods the chart creates.
const controllerSelector = "app=auto-agent-controller"

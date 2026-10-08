// Package config is the only place the agent reads environment variables
// (PLAN-002 task 8.1). Components receive values from Config; they never
// call os.Getenv themselves, which TestNoEnvReadsOutsideConfig enforces.
//
// Every variable is read unconditionally, so Names() can list them by
// running Load with a recording getter. docs/CONFIGURATION.md is checked
// against that list.
package config

import (
	"strconv"
	"strings"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// Agent roles (ADR-001).
const (
	RoleAll        = "all"        // one process does everything (local runs, tests)
	RoleNode       = "node"       // DaemonSet: own node only, forwards events
	RoleController = "controller" // Deployment: cluster loops, event log, API
)

// Getenv looks up one variable; os.Getenv in production, a map in tests.
type Getenv func(string) string

// Config is everything the agent reads from its environment.
type Config struct {
	// Policy holds mode, allowlist, scaling and timeout settings. It is the
	// part that is hot reloaded from the ConfigMap.
	Policy *policy.Policy

	// Identity, from the downward API.
	NodeName     string
	PodName      string
	PodNamespace string

	// API client rate limits, per pod (ISS-056).
	APIQPS   float32
	APIBurst int

	// Role and the node to controller link (ADR-001).
	Role          string // all, node or controller
	ControllerURL string // where a node agent sends its events
	InternalToken string // shared by node agents and the controller

	// Guardrails and leader election.
	BlastRadiusMaxNamespaces int
	CircuitBreakerThreshold  int
	QuietHours               string
	LeaderLeaseNamespace     string

	// Leader loop intervals.
	ScaleInterval  time.Duration
	JobInterval    time.Duration
	QuotaInterval  time.Duration
	HealthInterval time.Duration

	// Scale-up PromQL gates.
	ScalingGates ScalingGates

	// Metrics source.
	MetricsProvider string
	PrometheusURL   string

	// Notifications and diagnosis.
	SlackWebhookURL    string
	SlackSigningSecret string
	AlertmanagerURL    string
	LLMAPIURL          string
	LLMAPIKey          string
	LLMModel           string

	// Tickets and GitOps.
	TicketsEnabled  bool
	TicketsProvider string
	GitHubRepo      string
	GitHubToken     string
	JiraBaseURL     string
	JiraProjectKey  string
	JiraToken       string
	JiraEmail       string
	GitToken        string
	GitOpsRepo      string
	GitOpsBranch    string
	GitOpsProvider  string

	// Logs, audit and retention.
	Storage          Storage
	LogRetentionDays int
	AuditLogPath     string
	LogFormat        string

	// Security and dashboard.
	DashboardToken string
	TLSCertCheck   bool

	// Admission webhook.
	Webhook Webhook

	// Escalation (built, wired in PLAN-002 phase 16).
	Escalation Escalation

	// Learning mode.
	LearningEnabled    bool
	LearningPeriodDays int

	// Cost tab. cost.go still reads these itself until PLAN-002 task 9.3;
	// they are here so the variable list is complete.
	Cost Cost
}

// ScalingGates are optional PromQL queries that must return > 0 for scale-up.
type ScalingGates struct {
	QueueDepth string
	ErrorRate  string
	P95Latency string
}

// Configured reports whether any gate is set.
func (g ScalingGates) Configured() bool {
	return g.QueueDepth != "" || g.ErrorRate != "" || g.P95Latency != ""
}

// Storage selects where incident log bundles go.
type Storage struct {
	Store    string // efs | s3 | none; anything else means the filesystem
	EFSPath  string
	S3Bucket string
	S3Prefix string
}

// Webhook configures the admission webhook.
type Webhook struct {
	CertFile         string
	KeyFile          string
	RequireLimits    bool
	RequireReadiness bool
	BlockedImages    []string
}

// Enabled reports whether the agent should serve the webhook.
func (w Webhook) Enabled() bool { return w.CertFile != "" && w.KeyFile != "" }

// Escalation configures PagerDuty, OpsGenie and email.
type Escalation struct {
	PagerDutyRoutingKey string
	OpsGenieAPIKey      string
	SMTPHost            string
	SMTPPort            string
	SMTPUser            string
	SMTPPass            string
	SMTPFrom            string
	EmailTo             string
}

// Cost configures the Cost tab.
type Cost struct {
	CPUPerHour     string
	MemPerGiBHour  string
	Currency       string
	InstancePrices string
	KubecostURL    string
	OpenCostURL    string
}

// DefaultLogDir is the only writable path in the container.
const DefaultLogDir = "/var/log/auto-agent"

// Load reads every variable. It never fails: invalid values fall back to
// their defaults, as the agent always has.
func Load(get Getenv) Config {
	r := reader{get}
	return Config{
		Policy: policy.Load(policy.Getenv(get)),

		NodeName:     r.str("NODE_NAME", ""),
		PodName:      r.str("POD_NAME", ""),
		PodNamespace: r.str("POD_NAMESPACE", ""),

		APIQPS:   float32(r.positiveInt("KUBE_API_QPS", 50)),
		APIBurst: r.positiveInt("KUBE_API_BURST", 100),

		Role:          r.str("AGENT_ROLE", RoleAll),
		ControllerURL: r.str("CONTROLLER_URL", ""),
		InternalToken: r.str("INTERNAL_TOKEN", ""),

		BlastRadiusMaxNamespaces: r.positiveInt("BLAST_RADIUS_MAX_NAMESPACES", 5),
		CircuitBreakerThreshold:  r.positiveInt("CIRCUIT_BREAKER_THRESHOLD", 5),
		QuietHours:               r.str("QUIET_HOURS", ""),
		LeaderLeaseNamespace:     r.str("LEADER_LEASE_NAMESPACE", "kube-system"),

		ScaleInterval:  r.duration("SCALE_INTERVAL", 30*time.Second),
		JobInterval:    r.duration("JOB_INTERVAL", 2*time.Minute),
		QuotaInterval:  r.duration("QUOTA_INTERVAL", 5*time.Minute),
		HealthInterval: r.duration("HEALTH_INTERVAL", 3*time.Minute),

		ScalingGates: ScalingGates{
			QueueDepth: r.str("PROM_QUEUE_DEPTH", ""),
			ErrorRate:  r.str("PROM_ERROR_RATE", ""),
			P95Latency: r.str("PROM_P95_LATENCY", ""),
		},

		MetricsProvider: r.str("METRICS_PROVIDER", "metrics-server"),
		PrometheusURL:   r.str("PROMETHEUS_URL", ""),

		SlackWebhookURL:    r.str("SLACK_WEBHOOK_URL", ""),
		SlackSigningSecret: r.str("SLACK_SIGNING_SECRET", ""),
		AlertmanagerURL:    r.str("ALERTMANAGER_URL", ""),
		LLMAPIURL:          r.str("LLM_API_URL", ""),
		LLMAPIKey:          r.str("LLM_API_KEY", ""),
		LLMModel:           r.str("LLM_MODEL", ""),

		TicketsEnabled:  r.str("TICKETS_ENABLED", "") == "true",
		TicketsProvider: r.str("TICKETS_PROVIDER", ""),
		GitHubRepo:      r.str("GITHUB_REPO", ""),
		GitHubToken:     r.str("GITHUB_TOKEN", ""),
		JiraBaseURL:     r.str("JIRA_BASE_URL", ""),
		JiraProjectKey:  r.str("JIRA_PROJECT_KEY", ""),
		JiraToken:       r.str("JIRA_TOKEN", ""),
		JiraEmail:       r.str("JIRA_EMAIL", ""),
		GitToken:        r.str("GIT_TOKEN", ""),
		GitOpsRepo:      r.str("GITOPS_REPO", ""),
		GitOpsBranch:    r.str("GITOPS_BRANCH", ""),
		GitOpsProvider:  r.str("GITOPS_PROVIDER", "github"),

		Storage: Storage{
			Store:    r.str("LOG_STORE", ""),
			EFSPath:  r.str("LOG_EFS_PATH", DefaultLogDir),
			S3Bucket: r.str("LOG_S3_BUCKET", ""),
			S3Prefix: r.str("LOG_S3_PREFIX", ""),
		},
		LogRetentionDays: r.positiveInt("LOG_RETENTION_DAYS", 7),
		AuditLogPath:     r.str("AUDIT_LOG_PATH", DefaultLogDir+"/audit.jsonl"),
		LogFormat:        r.str("LOG_FORMAT", "text"),

		DashboardToken: r.str("DASHBOARD_TOKEN", ""),
		TLSCertCheck:   r.str("TLS_CERT_CHECK", "") == "true",

		Webhook: Webhook{
			CertFile:         r.str("WEBHOOK_CERT_FILE", ""),
			KeyFile:          r.str("WEBHOOK_KEY_FILE", ""),
			RequireLimits:    r.str("WEBHOOK_REQUIRE_LIMITS", "") != "false",
			RequireReadiness: r.str("WEBHOOK_REQUIRE_READINESS", "") != "false",
			BlockedImages:    r.list("WEBHOOK_BLOCKED_IMAGES"),
		},

		Escalation: Escalation{
			PagerDutyRoutingKey: r.str("PAGERDUTY_ROUTING_KEY", ""),
			OpsGenieAPIKey:      r.str("OPSGENIE_API_KEY", ""),
			SMTPHost:            r.str("SMTP_HOST", ""),
			SMTPPort:            r.str("SMTP_PORT", ""),
			SMTPUser:            r.str("SMTP_USER", ""),
			SMTPPass:            r.str("SMTP_PASS", ""),
			SMTPFrom:            r.str("SMTP_FROM", ""),
			EmailTo:             r.str("ESCALATION_EMAIL_TO", ""),
		},

		LearningEnabled:    r.str("LEARNING_ENABLED", "") == "true",
		LearningPeriodDays: r.positiveInt("LEARNING_PERIOD_DAYS", 14),

		Cost: Cost{
			CPUPerHour:     r.str("COST_CPU_PER_HOUR", ""),
			MemPerGiBHour:  r.str("COST_MEM_PER_GIB_HOUR", ""),
			Currency:       r.str("COST_CURRENCY", ""),
			InstancePrices: r.str("COST_INSTANCE_PRICES", ""),
			KubecostURL:    r.str("KUBECOST_URL", ""),
			OpenCostURL:    r.str("OPENCOST_URL", ""),
		},
	}
}

// Names lists every variable Load reads, in the order it reads them.
func Names() []string {
	var names []string
	seen := map[string]bool{}
	Load(func(k string) string {
		if !seen[k] {
			seen[k] = true
			names = append(names, k)
		}
		return ""
	})
	return names
}

type reader struct{ get Getenv }

func (r reader) str(key, def string) string {
	if v := r.get(key); v != "" {
		return v
	}
	return def
}

func (r reader) positiveInt(key string, def int) int {
	n, err := strconv.Atoi(r.get(key))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func (r reader) duration(key string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(r.get(key))
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func (r reader) list(key string) []string {
	var out []string
	for _, v := range strings.Split(r.get(key), ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// KlogVerbosity maps agent.logLevel (LOG_LEVEL) to a klog -v level:
// error, warn, info -> 0; debug -> 4; trace -> 6; a number 0 to 10 as is.
// Anything else returns ok=false and the caller keeps klog's default.
func KlogVerbosity(level string) (v int, ok bool) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "error", "warn", "warning", "info":
		return 0, true
	case "debug":
		return 4, true
	case "trace":
		return 6, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(level))
	if err != nil || n < 0 || n > 10 {
		return 0, false
	}
	return n, true
}

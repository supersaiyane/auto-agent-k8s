package policy

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

type Mode string

const (
	Observe Mode = "observe"
	Suggest Mode = "suggest"
	Fix     Mode = "fix"
	DryRun  Mode = "dry-run"
)

type Policy struct {
	Mode               Mode
	HPACoexistence     bool
	CPUThreshold       float64
	ScaleWindow        string
	MaxScaleStep       int
	MaxActionsPer10m   int
	NamespaceAllow     map[string]struct{}
	ExcludedAnnotation string
	LLMEnabled         bool
	LogLevel           string

	// Scaling
	CooldownUp   string
	CooldownDown string
	MaxReplicas  int32
	MinReplicas  int32

	// Deduplication
	DedupTTLSeconds int

	// Timeouts (seconds)
	LLMTimeoutSec   int
	SlackTimeoutSec int
}

// Getenv looks up one variable. internal/config passes os.Getenv; tests pass
// a map lookup.
type Getenv func(string) string

// Load builds the policy from variables read through get. Only
// internal/config calls it with the real environment (PLAN-002 task 8.3).
func Load(get Getenv) *Policy {
	g := envReader{get}
	ns := map[string]struct{}{}
	for _, n := range strings.Split(g.str("NAMESPACE_ALLOWLIST", "default"), ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			ns[n] = struct{}{}
		}
	}

	m := Mode(g.str("AUTO_MODE", string(DryRun)))
	switch m {
	case Observe, Suggest, Fix, DryRun:
	default:
		klog.Warningf("invalid AUTO_MODE %q, defaulting to observe", m)
		m = Observe
	}

	p := &Policy{
		Mode:               m,
		HPACoexistence:     g.bool("HPA_COEXISTENCE", true),
		CPUThreshold:       g.float("SCALE_CPU_THRESHOLD", 0.8),
		ScaleWindow:        g.str("SCALE_WINDOW", "5m"),
		MaxScaleStep:       g.int("MAX_SCALE_STEP", 2),
		MaxActionsPer10m:   g.int("MAX_ACTIONS_PER_10M", 10),
		NamespaceAllow:     ns,
		ExcludedAnnotation: g.str("EXCLUDED_ANNOTATION", "auto-agent.io/disable"),
		LLMEnabled:         g.bool("LLM_ENABLED", false),
		LogLevel:           g.str("LOG_LEVEL", "info"),
		CooldownUp:         g.str("COOLDOWN_UP", "2m"),
		CooldownDown:       g.str("COOLDOWN_DOWN", "10m"),
		MaxReplicas:        int32(g.int("MAX_REPLICAS", 50)),
		MinReplicas:        int32(g.int("MIN_REPLICAS", 1)),
		DedupTTLSeconds:    g.int("DEDUP_TTL_SECONDS", 300),
		LLMTimeoutSec:      g.int("LLM_TIMEOUT_SEC", 10),
		SlackTimeoutSec:    g.int("SLACK_TIMEOUT_SEC", 5),
	}
	if err := p.Validate(); err != nil {
		klog.Fatalf("invalid policy: %v", err)
	}
	return p
}

func (p *Policy) Validate() error {
	if len(p.NamespaceAllow) == 0 {
		return fmt.Errorf("NAMESPACE_ALLOWLIST must not be empty")
	}
	if p.CPUThreshold <= 0 || p.CPUThreshold > 1.0 {
		return fmt.Errorf("SCALE_CPU_THRESHOLD must be in (0, 1.0], got %f", p.CPUThreshold)
	}
	if p.MaxScaleStep < 1 {
		return fmt.Errorf("MAX_SCALE_STEP must be >= 1, got %d", p.MaxScaleStep)
	}
	if p.MaxActionsPer10m < 1 {
		return fmt.Errorf("MAX_ACTIONS_PER_10M must be >= 1, got %d", p.MaxActionsPer10m)
	}
	if p.MaxReplicas < p.MinReplicas {
		return fmt.Errorf("MAX_REPLICAS (%d) must be >= MIN_REPLICAS (%d)", p.MaxReplicas, p.MinReplicas)
	}
	return nil
}

// AllowedNamespace returns true if the namespace is in the allowlist.
func (p *Policy) AllowedNamespace(ns string) bool {
	_, ok := p.NamespaceAllow[ns]
	return ok
}

func parseNamespaceList(s string) map[string]struct{} {
	ns := map[string]struct{}{}
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			ns[n] = struct{}{}
		}
	}
	return ns
}

func envIntVal(s string, fallback int) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return i
}

func envFloatVal(s string, fallback float64) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return f
}

type envReader struct{ get Getenv }

func (r envReader) str(key, fallback string) string {
	if v := r.get(key); v != "" {
		return v
	}
	return fallback
}

func (r envReader) bool(key string, fallback bool) bool {
	v := r.get(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		klog.Warningf("invalid bool for %s=%q, using default %v", key, v, fallback)
		return fallback
	}
	return b
}

func (r envReader) float(key string, fallback float64) float64 {
	v := r.get(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		klog.Warningf("invalid float for %s=%q, using default %f", key, v, fallback)
		return fallback
	}
	return f
}

func (r envReader) int(key string, fallback int) int {
	v := r.get(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		klog.Warningf("invalid int for %s=%q, using default %d", key, v, fallback)
		return fallback
	}
	return i
}

// StaticSource serves one fixed policy, for code paths with no hot reload
// (tests and one-off tools). Production code reads through HotReloader.
type StaticSource struct{ P *Policy }

// Get returns the fixed policy.
func (s StaticSource) Get() *Policy { return s.P }

// Static wraps p as a policy source.
func Static(p *Policy) StaticSource { return StaticSource{P: p} }

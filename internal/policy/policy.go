package policy

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

type Mode string

const (
	Observe Mode = "observe"
	Suggest Mode = "suggest"
	Fix     Mode = "fix"
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
	CooldownUp    string
	CooldownDown  string
	MaxReplicas   int32
	MinReplicas   int32

	// Deduplication
	DedupTTLSeconds int

	// Timeouts (seconds)
	LLMTimeoutSec   int
	SlackTimeoutSec int
}

func LoadFromEnv() *Policy {
	ns := map[string]struct{}{}
	for _, n := range strings.Split(envOr("NAMESPACE_ALLOWLIST", "default"), ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			ns[n] = struct{}{}
		}
	}

	m := Mode(envOr("AUTO_MODE", "fix"))
	switch m {
	case Observe, Suggest, Fix:
	default:
		klog.Warningf("invalid AUTO_MODE %q, defaulting to observe", m)
		m = Observe
	}

	p := &Policy{
		Mode:               m,
		HPACoexistence:     envBool("HPA_COEXISTENCE", true),
		CPUThreshold:       envFloat("SCALE_CPU_THRESHOLD", 0.8),
		ScaleWindow:        envOr("SCALE_WINDOW", "5m"),
		MaxScaleStep:       envInt("MAX_SCALE_STEP", 2),
		MaxActionsPer10m:   envInt("MAX_ACTIONS_PER_10M", 10),
		NamespaceAllow:     ns,
		ExcludedAnnotation: envOr("EXCLUDED_ANNOTATION", "auto-agent.io/disable"),
		LLMEnabled:         envBool("LLM_ENABLED", false),
		LogLevel:           envOr("LOG_LEVEL", "info"),
		CooldownUp:         envOr("COOLDOWN_UP", "2m"),
		CooldownDown:       envOr("COOLDOWN_DOWN", "10m"),
		MaxReplicas:        int32(envInt("MAX_REPLICAS", 50)),
		MinReplicas:        int32(envInt("MIN_REPLICAS", 1)),
		DedupTTLSeconds:    envInt("DEDUP_TTL_SECONDS", 300),
		LLMTimeoutSec:      envInt("LLM_TIMEOUT_SEC", 10),
		SlackTimeoutSec:    envInt("SLACK_TIMEOUT_SEC", 5),
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
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

func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
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

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
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

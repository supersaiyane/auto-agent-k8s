package policy

import (
	"os"
	"testing"
)

func TestLoadFromEnv_Defaults(t *testing.T) {
	// Clear env
	for _, k := range []string{"AUTO_MODE", "NAMESPACE_ALLOWLIST", "SCALE_CPU_THRESHOLD",
		"MAX_SCALE_STEP", "MAX_ACTIONS_PER_10M", "HPA_COEXISTENCE", "LLM_ENABLED",
		"COOLDOWN_UP", "COOLDOWN_DOWN", "MAX_REPLICAS", "MIN_REPLICAS",
		"DEDUP_TTL_SECONDS", "LLM_TIMEOUT_SEC", "SLACK_TIMEOUT_SEC",
		"EXCLUDED_ANNOTATION", "LOG_LEVEL", "SCALE_WINDOW"} {
		os.Unsetenv(k)
	}
	// Set minimum required
	os.Setenv("NAMESPACE_ALLOWLIST", "default")
	defer os.Unsetenv("NAMESPACE_ALLOWLIST")

	p := LoadFromEnv()

	if p.Mode != Fix {
		t.Errorf("expected mode Fix, got %s", p.Mode)
	}
	if p.CPUThreshold != 0.8 {
		t.Errorf("expected CPU threshold 0.8, got %f", p.CPUThreshold)
	}
	if p.MaxScaleStep != 2 {
		t.Errorf("expected max scale step 2, got %d", p.MaxScaleStep)
	}
	if p.MaxActionsPer10m != 10 {
		t.Errorf("expected max actions 10, got %d", p.MaxActionsPer10m)
	}
	if !p.AllowedNamespace("default") {
		t.Error("expected 'default' in allowed namespaces")
	}
	if p.AllowedNamespace("kube-system") {
		t.Error("expected 'kube-system' not in allowed namespaces")
	}
	if p.MaxReplicas != 50 {
		t.Errorf("expected max replicas 50, got %d", p.MaxReplicas)
	}
}

func TestLoadFromEnv_CustomValues(t *testing.T) {
	os.Setenv("AUTO_MODE", "observe")
	os.Setenv("NAMESPACE_ALLOWLIST", "prod,staging")
	os.Setenv("SCALE_CPU_THRESHOLD", "0.7")
	os.Setenv("MAX_SCALE_STEP", "3")
	os.Setenv("MAX_ACTIONS_PER_10M", "5")
	os.Setenv("MAX_REPLICAS", "100")
	os.Setenv("MIN_REPLICAS", "2")

	defer func() {
		for _, k := range []string{"AUTO_MODE", "NAMESPACE_ALLOWLIST", "SCALE_CPU_THRESHOLD",
			"MAX_SCALE_STEP", "MAX_ACTIONS_PER_10M", "MAX_REPLICAS", "MIN_REPLICAS"} {
			os.Unsetenv(k)
		}
	}()

	p := LoadFromEnv()

	if p.Mode != Observe {
		t.Errorf("expected mode Observe, got %s", p.Mode)
	}
	if !p.AllowedNamespace("prod") {
		t.Error("expected 'prod' in allowed namespaces")
	}
	if !p.AllowedNamespace("staging") {
		t.Error("expected 'staging' in allowed namespaces")
	}
	if p.CPUThreshold != 0.7 {
		t.Errorf("expected CPU threshold 0.7, got %f", p.CPUThreshold)
	}
	if p.MaxReplicas != 100 {
		t.Errorf("expected max replicas 100, got %d", p.MaxReplicas)
	}
	if p.MinReplicas != 2 {
		t.Errorf("expected min replicas 2, got %d", p.MinReplicas)
	}
}

func TestValidate_InvalidCPUThreshold(t *testing.T) {
	p := &Policy{
		NamespaceAllow:   map[string]struct{}{"default": {}},
		CPUThreshold:     1.5,
		MaxScaleStep:     1,
		MaxActionsPer10m: 10,
		MaxReplicas:      50,
		MinReplicas:      1,
	}
	if err := p.Validate(); err == nil {
		t.Error("expected validation error for CPU threshold > 1.0")
	}
}

func TestValidate_EmptyNamespaces(t *testing.T) {
	p := &Policy{
		NamespaceAllow:   map[string]struct{}{},
		CPUThreshold:     0.8,
		MaxScaleStep:     1,
		MaxActionsPer10m: 10,
		MaxReplicas:      50,
		MinReplicas:      1,
	}
	if err := p.Validate(); err == nil {
		t.Error("expected validation error for empty namespace allowlist")
	}
}

func TestValidate_MaxLessThanMin(t *testing.T) {
	p := &Policy{
		NamespaceAllow:   map[string]struct{}{"default": {}},
		CPUThreshold:     0.8,
		MaxScaleStep:     1,
		MaxActionsPer10m: 10,
		MaxReplicas:      2,
		MinReplicas:      5,
	}
	if err := p.Validate(); err == nil {
		t.Error("expected validation error for max < min replicas")
	}
}

func TestValidate_Valid(t *testing.T) {
	p := &Policy{
		NamespaceAllow:   map[string]struct{}{"default": {}},
		CPUThreshold:     0.8,
		MaxScaleStep:     2,
		MaxActionsPer10m: 10,
		MaxReplicas:      50,
		MinReplicas:      1,
	}
	if err := p.Validate(); err != nil {
		t.Errorf("expected no validation error, got: %v", err)
	}
}

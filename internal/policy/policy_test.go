package policy

import (
	"testing"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestLoad_Defaults(t *testing.T) {
	p := Load(env(nil))

	if p.Mode != DryRun {
		t.Errorf("expected default mode DryRun (CLAUDE.md constraint 2), got %s", p.Mode)
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

func TestLoad_CustomValues(t *testing.T) {
	p := Load(env(map[string]string{
		"AUTO_MODE": "observe", "NAMESPACE_ALLOWLIST": "prod,staging", "SCALE_CPU_THRESHOLD": "0.7",
		"MAX_SCALE_STEP": "3", "MAX_ACTIONS_PER_10M": "5", "MAX_REPLICAS": "100", "MIN_REPLICAS": "2",
	}))

	if p.Mode != Observe {
		t.Errorf("expected mode Observe, got %s", p.Mode)
	}
	if !p.AllowedNamespace("prod") || !p.AllowedNamespace("staging") {
		t.Error("expected 'prod' and 'staging' in allowed namespaces")
	}
	if p.CPUThreshold != 0.7 {
		t.Errorf("expected CPU threshold 0.7, got %f", p.CPUThreshold)
	}
	if p.MaxReplicas != 100 || p.MinReplicas != 2 {
		t.Errorf("expected replicas 2..100, got %d..%d", p.MinReplicas, p.MaxReplicas)
	}
}

func TestLoad_InvalidValuesFallBack(t *testing.T) {
	p := Load(env(map[string]string{
		"AUTO_MODE": "bogus", "HPA_COEXISTENCE": "maybe", "SCALE_CPU_THRESHOLD": "high", "MAX_SCALE_STEP": "two",
	}))
	if p.Mode != Observe {
		t.Errorf("invalid AUTO_MODE must fall back to observe, got %s", p.Mode)
	}
	if !p.HPACoexistence || p.CPUThreshold != 0.8 || p.MaxScaleStep != 2 {
		t.Errorf("invalid values must fall back to defaults, got hpa=%v cpu=%v step=%d", p.HPACoexistence, p.CPUThreshold, p.MaxScaleStep)
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

package config

import (
	"testing"
	"time"
)

func env(m map[string]string) Getenv { return func(k string) string { return m[k] } }

func TestLoad_Defaults(t *testing.T) {
	c := Load(env(nil))
	checks := []struct {
		name      string
		got, want any
	}{
		{"mode", string(c.Policy.Mode), "dry-run"},
		{"blast radius", c.BlastRadiusMaxNamespaces, 5},
		{"breaker", c.CircuitBreakerThreshold, 5},
		{"lease namespace", c.LeaderLeaseNamespace, "kube-system"},
		{"scale interval", c.ScaleInterval, 30 * time.Second},
		{"job interval", c.JobInterval, 2 * time.Minute},
		{"quota interval", c.QuotaInterval, 5 * time.Minute},
		{"health interval", c.HealthInterval, 3 * time.Minute},
		{"metrics provider", c.MetricsProvider, "metrics-server"},
		{"gitops provider", c.GitOpsProvider, "github"},
		{"efs path", c.Storage.EFSPath, DefaultLogDir},
		{"audit path", c.AuditLogPath, DefaultLogDir + "/audit.jsonl"},
		{"retention", c.LogRetentionDays, 7},
		{"log format", c.LogFormat, "text"},
		{"learning period", c.LearningPeriodDays, 14},
		{"webhook limits", c.Webhook.RequireLimits, true},
		{"webhook readiness", c.Webhook.RequireReadiness, true},
		{"tickets", c.TicketsEnabled, false},
		{"tls check", c.TLSCertCheck, false},
		{"gates", c.ScalingGates.Configured(), false},
		{"webhook enabled", c.Webhook.Enabled(), false},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s: got %v, want %v", ck.name, ck.got, ck.want)
		}
	}
}

func TestLoad_ValidValues(t *testing.T) {
	c := Load(env(map[string]string{
		"BLAST_RADIUS_MAX_NAMESPACES": "9", "SCALE_INTERVAL": "45s", "TICKETS_ENABLED": "true",
		"TLS_CERT_CHECK": "true", "WEBHOOK_REQUIRE_LIMITS": "false", "PROM_ERROR_RATE": "up",
		"WEBHOOK_CERT_FILE": "/c", "WEBHOOK_KEY_FILE": "/k", "NAMESPACE_ALLOWLIST": "a,b",
	}))
	if c.BlastRadiusMaxNamespaces != 9 || c.ScaleInterval != 45*time.Second || !c.TicketsEnabled ||
		!c.TLSCertCheck || c.Webhook.RequireLimits || !c.ScalingGates.Configured() || !c.Webhook.Enabled() ||
		!c.Policy.AllowedNamespace("b") {
		t.Fatalf("valid values not applied: %+v", c)
	}
}

func TestLoad_InvalidValuesFallBack(t *testing.T) {
	c := Load(env(map[string]string{
		"BLAST_RADIUS_MAX_NAMESPACES": "-3", "CIRCUIT_BREAKER_THRESHOLD": "lots",
		"SCALE_INTERVAL": "soon", "JOB_INTERVAL": "-1m", "LOG_RETENTION_DAYS": "0",
		"LEARNING_PERIOD_DAYS": "x",
	}))
	if c.BlastRadiusMaxNamespaces != 5 || c.CircuitBreakerThreshold != 5 || c.ScaleInterval != 30*time.Second ||
		c.JobInterval != 2*time.Minute || c.LogRetentionDays != 7 || c.LearningPeriodDays != 14 {
		t.Fatalf("invalid values must fall back to defaults: %+v", c)
	}
}

// ISS-041: an unset list must be empty. The old code produced [""], and the
// webhook blocks any image starting with an entry, so "" blocked every image.
func TestLoad_BlockedImagesUnsetIsEmpty(t *testing.T) {
	for _, v := range []string{"", " ", ",", " , "} {
		if got := Load(env(map[string]string{"WEBHOOK_BLOCKED_IMAGES": v})).Webhook.BlockedImages; len(got) != 0 {
			t.Errorf("WEBHOOK_BLOCKED_IMAGES=%q gave %q, want empty", v, got)
		}
	}
	got := Load(env(map[string]string{"WEBHOOK_BLOCKED_IMAGES": "docker.io/evil/, quay.io/bad "})).Webhook.BlockedImages
	if len(got) != 2 || got[0] != "docker.io/evil/" || got[1] != "quay.io/bad" {
		t.Errorf("got %q", got)
	}
}

func TestNames_ListsEveryVariableOnce(t *testing.T) {
	n := Names()
	seen := map[string]bool{}
	for _, v := range n {
		if seen[v] {
			t.Errorf("%s listed twice", v)
		}
		seen[v] = true
	}
	for _, must := range []string{"AUTO_MODE", "NAMESPACE_ALLOWLIST", "DASHBOARD_TOKEN", "WEBHOOK_BLOCKED_IMAGES", "OPENCOST_URL"} {
		if !seen[must] {
			t.Errorf("Names() is missing %s", must)
		}
	}
	t.Logf("%d variables", len(n))
}

// ISS-032: agent.logLevel was read but never used.
func TestKlogVerbosity(t *testing.T) {
	for _, tc := range []struct {
		in   string
		v    int
		okay bool
	}{{"info", 0, true}, {"", 0, true}, {"WARN", 0, true}, {"debug", 4, true}, {"trace", 6, true},
		{"7", 7, true}, {"11", 0, false}, {"-1", 0, false}, {"loud", 0, false}} {
		v, ok := KlogVerbosity(tc.in)
		if v != tc.v || ok != tc.okay {
			t.Errorf("KlogVerbosity(%q) = %d, %v; want %d, %v", tc.in, v, ok, tc.v, tc.okay)
		}
	}
}

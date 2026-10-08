package config

import (
	"strings"
	"testing"
)

// ISS-056: a misspelled key is named; platform variables are not.
func TestUnknownKeys(t *testing.T) {
	got := UnknownKeys([]string{
		"AUTO_MODE=fix", "AUTO_MOD=fix", "FIX_NAMESPACE=a", "lowercase=x", "PATH=/bin", "HOME=/root",
		"KUBERNETES_SERVICE_HOST=10.0.0.1", "KUBERNETES_SERVICE_PORT_HTTPS=443", "KUBERNETES_PORT=tcp://10.0.0.1:443",
		"KUBERNETES_PORT_443_TCP_ADDR=10.0.0.1", "LC_ALL=C", "KUBECONFIG=/k", "NOEQUALS", "GITOPS_MODE=pr",
	})
	if strings.Join(got, ",") != "AUTO_MOD,FIX_NAMESPACE,NOEQUALS" {
		t.Fatalf("unknown keys: %v", got)
	}
}

func TestDescribe(t *testing.T) {
	env := map[string]string{"AUTO_MODE": "fix", "LLM_API_KEY": "sk-secret", "INTERNAL_TOKEN": "t0k3n", "SMTP_PASSWORD": "pw"}
	lines := Describe(func(k string) string { return env[k] })
	joined := strings.Join(lines, "\n")
	if len(lines) != len(Names()) || !strings.Contains(joined, "AUTO_MODE=fix") || !strings.Contains(joined, "LLM_API_KEY=(set, redacted)") ||
		!strings.Contains(joined, "QUIET_HOURS=(unset: default)") {
		t.Fatalf("describe:\n%s", joined)
	}
	for _, secret := range []string{"sk-secret", "t0k3n", "pw\n"} {
		if strings.Contains(joined+"\n", secret) {
			t.Fatalf("describe printed %q", secret)
		}
	}
}

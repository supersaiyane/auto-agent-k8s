package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// settingName is the shape of an agent setting.
var settingName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// secretName matches settings whose value is never printed.
var secretName = regexp.MustCompile(`TOKEN|SECRET|PASSWORD|API_KEY|ROUTING_KEY|WEBHOOK_URL|_DSN$`)

// platformName matches variables the container runtime and Kubernetes set
// (service links such as API_SERVICE_HOST and API_PORT_8080_TCP), which are
// not agent settings.
var platformName = regexp.MustCompile(`^(PATH|HOME|HOSTNAME|TERM|SHLVL|PWD|OLDPWD|USER|SHELL|TMPDIR|LANG|LC_[A-Z]+|TZ|KUBECONFIG|SSL_CERT_(FILE|DIR))$|_SERVICE_(HOST|PORT)(_[A-Z0-9_]+)?$|_PORT(_[0-9]+_(TCP|UDP|SCTP)(_[A-Z]+)?)?$`)

// Environ returns the process environment as KEY=value entries. Like
// os.Getenv, it is read only through this package (PLAN-002 8.1).
func Environ() []string { return os.Environ() }

// Reserved lists the keys the chart sets for features not built yet (owner
// decision 2026-10-07, PLAN-002 phase 17). They are neither read nor
// unknown. docs/CONFIGURATION.md lists the same keys, which a test checks.
func Reserved() []string {
	return []string{"ANOMALIES_POLL_INTERVAL", "GITOPS_MODE", "GITOPS_VALUES_FILE", "GITOPS_AUTHOR_NAME",
		"GITOPS_AUTHOR_EMAIL", "IMAGE_MIRROR_ENABLED", "IMAGE_MIRROR_PREFIX", "IMAGE_MIRROR_ALLOWLIST"}
}

// UnknownKeys names the settings in env ("KEY=value" entries, as from
// os.Environ) that the agent does not read, so a misspelled key is reported
// instead of silently ignored (ISS-056). Platform variables are skipped.
func UnknownKeys(env []string) []string {
	known := map[string]bool{}
	for _, n := range append(Names(), Reserved()...) {
		known[n] = true
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if settingName.MatchString(k) && !known[k] && !platformName.MatchString(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Describe lists every setting the agent reads with its value from get,
// secrets redacted and unset ones marked, for auto-agent check-config.
func Describe(get Getenv) []string {
	var out []string
	for _, n := range Names() {
		v := get(n)
		switch {
		case v == "":
			v = "(unset: default)"
		case secretName.MatchString(n):
			v = "(set, redacted)"
		}
		out = append(out, fmt.Sprintf("%s=%s", n, v))
	}
	return out
}

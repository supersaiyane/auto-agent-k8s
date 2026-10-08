package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
)

// userDocs are the pages that describe the agent as it is. Plans, ADRs,
// tasks and the checkpoint are history and may name what no longer exists.
func userDocs(t *testing.T) []string {
	t.Helper()
	pages, err := filepath.Glob("../../docs/wiki/*.md")
	if err != nil {
		t.Fatal(err)
	}
	return append(pages, "../../docs/GUIDE.md", "../../docs/CONFIGURATION.md", "../../README.md")
}

// code spans in markdown: `...`
var codeSpan = regexp.MustCompile("`([^`\n]+)`")

var (
	settingLike = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)
	routeLike   = regexp.MustCompile(`(?:GET |POST |PUT |DELETE )?(/api/[a-z0-9/<>_-]*)`)
	valueLike   = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-zA-Z][a-zA-Z0-9]*)+$`)
	// helmSet is the key of a --set flag in any doc command.
	helmSet = regexp.MustCompile(`--set(?:-string)?[ =]"?([a-zA-Z][a-zA-Z0-9.]*)(?:\[[0-9]+\])?[.=]`)
)

// notSettings are upper-case names with an underscore that the docs use
// for something other than an agent setting.
// fileName matches spans that name a file, not a value.
var fileName = regexp.MustCompile(`\.(go|ya?ml|md|js|mjs|css|html|json|sh|txt)$`)

// externalRoutes are other products' APIs the docs mention.
var externalRoutes = map[string]bool{"/api/v2/alerts": true} // Alertmanager

var notSettings = map[string]bool{
	"POD_IP": true, "HOST_IP": true, "GOMAXPROCS": true, "TLS_CERT": true,
}

// ISS-057: a page that names a setting, an API route or a Helm value that
// does not exist fails, so the docs cannot drift from the code.
func TestDocs_NameOnlyWhatExists(t *testing.T) {
	settings := map[string]bool{}
	for _, n := range append(config.Names(), config.Reserved()...) {
		settings[n] = true
	}
	values := helmValues(t)
	crdFields := policyFields(t)
	var problems []string
	for _, page := range userDocs(t) {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimPrefix(page, "../../")
		for _, m := range helmSet.FindAllStringSubmatch(string(body), -1) {
			if !values[m[1]] {
				problems = append(problems, name+": helm --set "+m[1])
			}
		}
		for _, m := range codeSpan.FindAllStringSubmatch(string(body), -1) {
			span := strings.TrimSpace(m[1])
			for _, tok := range strings.FieldsFunc(span, func(r rune) bool { return strings.ContainsRune(" =:,()[]{}\"'|", r) }) {
				if settingLike.MatchString(tok) && !settings[tok] && !notSettings[tok] {
					problems = append(problems, name+": setting "+tok)
				}
			}
			for _, r := range routeLike.FindAllStringSubmatch(span, -1) {
				if !knownRoute(r[1]) {
					problems = append(problems, name+": route "+r[1])
				}
			}
			if valueLike.MatchString(span) && !fileName.MatchString(span) && !crdFields[span] {
				if top := strings.SplitN(span, ".", 2)[0]; values[top] && !values[span] {
					problems = append(problems, name+": Helm value "+span)
				}
			}
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Errorf("the docs name what does not exist (%d):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// knownRoute reports whether p (placeholders like <ns> allowed) is served.
func knownRoute(p string) bool {
	if externalRoutes[p] {
		return true
	}
	p = strings.TrimRight(p, "/")
	if p == "/api" {
		return true // the prefix itself
	}
	for _, r := range apiRoutes() {
		if p == strings.TrimRight(r, "/") || (strings.HasSuffix(r, "/") && strings.HasPrefix(p, r)) {
			return true
		}
	}
	return false
}

// helmValues flattens values.yaml into dotted paths, top-level keys included.
func helmValues(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../charts/auto-agent/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, val := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			if sub, ok := val.(map[string]any); ok {
				walk(p, sub)
			}
		}
	}
	walk("", v)
	return out
}

// policyFields flattens the AutoRemediationPolicy spec schema into dotted
// paths, so a CRD field that shares a name with a Helm value is accepted.
func policyFields(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../../charts/auto-agent/crds/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRD files: %v", err)
	}
	out := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Spec struct {
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema schemaNode `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		for _, v := range crd.Spec.Versions {
			if spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]; ok {
				spec.flatten("", out)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the CRD schema has no spec fields; the parser is broken")
	}
	return out
}

type schemaNode struct {
	Properties map[string]schemaNode `json:"properties"`
}

func (n schemaNode) flatten(prefix string, out map[string]bool) {
	for k, sub := range n.Properties {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		out[p] = true
		sub.flatten(p, out)
	}
}

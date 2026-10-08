package config

import (
	"bufio"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

// envHelper matches any function that reads an environment variable:
// os.Getenv, os.LookupEnv and the project's envOr, envBool, intEnv, ...
var envHelper = regexp.MustCompile(`(?i)env`)

// codeEnvVars returns every variable name passed as a string literal to an
// env-reading function in non-test code. After PLAN-002 task 8.3 this only
// finds reads outside the config package (allowed only in cost.go until 9.3);
// it is a cross-check that nothing reads a variable Names() does not list.
func codeEnvVars(t *testing.T) map[string]bool {
	t.Helper()
	vars := map[string]bool{}
	fset := token.NewFileSet()
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				var name string
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !envHelper.MatchString(name) {
					return true
				}
				if v, err := strconv.Unquote(lit.Value); err == nil && envName.MatchString(v) {
					vars[v] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return vars
}

// docEnvVars returns the variables listed in the reference tables, and
// separately those listed under "Set by the chart but not read".
func docEnvVars(t *testing.T) (listed, unread map[string]bool) {
	t.Helper()
	f, err := os.Open("../../docs/CONFIGURATION.md")
	if err != nil {
		t.Fatalf("open reference: %v", err)
	}
	defer f.Close()
	listed, unread = map[string]bool{}, map[string]bool{}
	section := "main"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## Set by the chart but not read"):
			section = "unread"
		case strings.HasPrefix(line, "## Chart values that shape"):
			section = "shape"
		}
		if !strings.HasPrefix(line, "| `") || section == "shape" {
			continue
		}
		cell := strings.TrimSpace(strings.Split(line, "|")[1])
		v := strings.Trim(cell, "`")
		if !envName.MatchString(v) {
			continue
		}
		if section == "unread" {
			unread[v] = true
		} else {
			listed[v] = true
		}
	}
	return listed, unread
}

// chartEnvVars renders the chart and returns every variable it sets: ConfigMap
// data, Secret stringData and DaemonSet container env.
func chartEnvVars(t *testing.T) map[string]bool {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; the chart half of this check needs `helm template`")
	}
	out, err := exec.Command("helm", "template", "auto-agent", "../../charts/auto-agent").CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	vars := map[string]bool{}
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var obj map[string]any
		if err := dec.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode chart output: %v", err)
		}
		for _, key := range []string{"data", "stringData"} {
			if m, ok := obj[key].(map[string]any); ok && (obj["kind"] == "ConfigMap" || obj["kind"] == "Secret") {
				for k := range m {
					vars[k] = true
				}
			}
		}
		if obj["kind"] == "DaemonSet" {
			spec, _ := obj["spec"].(map[string]any)
			tmpl, _ := spec["template"].(map[string]any)
			pod, _ := tmpl["spec"].(map[string]any)
			containers, _ := pod["containers"].([]any)
			for _, c := range containers {
				env, _ := c.(map[string]any)["env"].([]any)
				for _, e := range env {
					if name, ok := e.(map[string]any)["name"].(string); ok {
						vars[name] = true
					}
				}
			}
		}
	}
	return vars
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestConfigReference_MatchesCodeAndChart keeps docs/CONFIGURATION.md, the
// code and the chart in step: every variable the agent reads (Names(), taken
// from Load itself) is documented, nothing documented is stale, and every
// variable the chart sets is either read or listed as not read.
func TestConfigReference_MatchesCodeAndChart(t *testing.T) {
	code := map[string]bool{}
	for _, n := range Names() {
		code[n] = true
	}
	if len(code) < 50 {
		t.Fatalf("Names() returned only %d variables; Load is not reading them all", len(code))
	}
	for v := range codeEnvVars(t) {
		if !code[v] {
			t.Errorf("%s is read outside internal/config and is missing from config.Load", v)
		}
	}
	listed, unread := docEnvVars(t)

	missing, stale := map[string]bool{}, map[string]bool{}
	for v := range code {
		if !listed[v] {
			missing[v] = true
		}
	}
	for v := range listed {
		if !code[v] {
			stale[v] = true
		}
	}
	if len(missing) > 0 {
		t.Errorf("read by code but not in docs/CONFIGURATION.md: %v", sorted(missing))
	}
	if len(stale) > 0 {
		t.Errorf("in docs/CONFIGURATION.md but no longer read by code: %v", sorted(stale))
	}
	for v := range unread {
		if code[v] {
			t.Errorf("%s is listed as not read, but the code reads it; move it to its section", v)
		}
	}
	reserved := map[string]bool{}
	for _, v := range Reserved() {
		reserved[v] = true
		if !unread[v] {
			t.Errorf("config.Reserved names %s, which docs/CONFIGURATION.md does not list as not read", v)
		}
	}
	for v := range unread {
		if !reserved[v] {
			t.Errorf("docs/CONFIGURATION.md lists %s as not read, but config.Reserved does not, so check-config would call it unknown", v)
		}
	}

	chart := chartEnvVars(t)
	undocumented := map[string]bool{}
	for v := range chart {
		if !code[v] && !unread[v] {
			undocumented[v] = true
		}
	}
	if len(undocumented) > 0 {
		t.Errorf("chart sets variables nothing reads and the reference does not list as unread: %v", sorted(undocumented))
	}
	for v := range unread {
		if !chart[v] {
			t.Errorf("%s is listed as set by the chart but the chart no longer sets it", v)
		}
	}
	t.Logf("%d variables read by code, %d set by the chart, %d documented as not read", len(code), len(chart), len(unread))
}

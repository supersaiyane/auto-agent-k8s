package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// envReadAllowed lists files that may still read the environment directly,
// each with the PLAN-002 task that removes it. Empty since 9.3.
var envReadAllowed = map[string]string{}

// TestNoEnvReadsOutsideConfig enforces PLAN-002 task 8.3: only this package
// reads environment variables; everything else receives values from Config.
func TestNoEnvReadsOutsideConfig(t *testing.T) {
	fset := token.NewFileSet()
	envFuncs := map[string]bool{"Getenv": true, "LookupEnv": true, "Environ": true}
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if strings.HasPrefix(rel, "internal/config/") || envReadAllowed[rel] != "" {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !envFuncs[sel.Sel.Name] {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "os" || pkg.Name == "syscall") {
					t.Errorf("%s: %s.%s outside internal/config; read it in config.Load and pass the value in",
						fset.Position(call.Pos()), pkg.Name, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// allowedGlobals are the only package-level variables allowed in non-test
// code, each with the reason it is not mutable shared state (PLAN-002 9.4).
// Anything else must be a field on a struct passed in at construction.
var allowedGlobals = map[string]string{
	"cmd/auto-agent/main.go:version":                     "set once at build time by -ldflags",
	"internal/crd/controller.go:gvr":                     "constant GroupVersionResource",
	"internal/httpapi/cost.go:awsPricing":                "read-only price table",
	"internal/httpapi/http.go:uiFS":                      "embedded static files",
	"internal/httpapi/http.go:apiRouteTable":             "read-only route table, walked by the auth tests",
	"internal/httpapi/terminal.go:clusterScoped":         "read-only lookup",
	"internal/kube/pod_extended.go:additionalPodReasons": "read-only lookup",
	"internal/kube/workloads.go:evictedReasons":          "read-only lookup",
	"internal/kube/findings.go:rungNames":                "read-only lookup",
	"internal/kube/lifecycle.go:finalizerOwners":         "read-only lookup",
	"internal/kube/promchecks.go:etcdChecks":             "read-only lookup",
	"internal/kube/promchecks.go:apiReplacements":        "read-only lookup",
	"internal/metrics/provider.go:ErrNoPromQL":           "sentinel error",
	"internal/kube/scope_settings.go:ErrOutsideCeiling":  "sentinel error",
	"internal/kube/pod_extended.go:ephemeralMarkers":     "read-only lookup",
	"internal/kube/security.go:limitRangeRefusal":        "compiled regexp",
	"internal/kube/security.go:webhookRefusal":           "compiled regexp",
	"internal/kube/security.go:rbacRefusal":              "compiled regexp",
	"internal/config/check.go:settingName":               "compiled regexp",
	"internal/config/check.go:secretName":                "compiled regexp",
	"internal/config/check.go:platformName":              "compiled regexp",
	"internal/kube/handlers.go:pullCauses":               "read-only lookup",
	"internal/kube/scheduling.go:nodeCountPrefix":        "compiled regexp",
	"internal/kube/scheduling.go:schedKinds":             "read-only lookup",
	"internal/kube/podstate.go:volumeInMessage":          "compiled regexp",
	"internal/kube/podstate.go:preemptedBy":              "compiled regexp",
	"internal/metrics/provider.go:unsafePromQL":          "compiled regexp",
	"internal/obs/apierrors.go:warnedForbidden":          "process-wide warn-once cache for forbidden reads; safe for concurrent use",
	"internal/obs/metrics.go:*":                          "Prometheus collectors, registered once per process by design",
	"internal/redact/redact.go:rules":                    "read-only redaction rules",
	"internal/webhook/validator.go:scheme":               "decoder scheme, built once",
	"internal/webhook/validator.go:codecs":               "decoder factory, built once",
}

// TestNoMutablePackageGlobals fails on any package-level var not listed in
// allowedGlobals, so shared state cannot creep back in (ISS-015).
func TestNoMutablePackageGlobals(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						if name.Name == "_" {
							continue
						}
						found++
						if allowedGlobals[rel+":"+name.Name] == "" && allowedGlobals[rel+":*"] == "" {
							t.Errorf("%s: package-level var %s; make it a field passed in at construction, or add it to allowedGlobals with a reason",
								fset.Position(name.Pos()), name.Name)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if found == 0 {
		t.Fatal("found no package-level vars at all; the scanner is broken")
	}
}

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
// each with the PLAN-002 task that removes it.
var envReadAllowed = map[string]string{
	"internal/httpapi/cost.go": "PLAN-002 9.3 moves cost settings into the server",
}

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

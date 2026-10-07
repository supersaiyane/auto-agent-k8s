package kube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mutatingVerbs are client-go methods that change cluster state.
var mutatingVerbs = map[string]bool{
	"Create": true, "Update": true, "UpdateStatus": true, "UpdateScale": true,
	"Patch": true, "Apply": true, "ApplyStatus": true, "ApplyScale": true,
	"Delete": true, "DeleteCollection": true, "Evict": true, "EvictV1": true, "EvictV1beta1": true,
}

// groupVersion matches typed client accessors such as CoreV1 or AutoscalingV2beta2.
var groupVersion = regexp.MustCompile(`^[A-Z][A-Za-z]*V[0-9]+((alpha|beta)[0-9]+)?$`)

// gateEntryPoints are the functions whose closure arguments may mutate.
var gateEntryPoints = map[string]bool{"applyMutation": true, "tryFixAction": true}

// isClientMutation reports whether call is <...>.XxxV1().<Resource>(...).<Verb>(...).
func isClientMutation(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !mutatingVerbs[sel.Sel.Name] {
		return false
	}
	for x := sel.X; ; {
		c, ok := x.(*ast.CallExpr)
		if !ok {
			return false
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if groupVersion.MatchString(s.Sel.Name) {
			return true
		}
		x = s.X
	}
}

// insideGate reports whether the innermost enclosing function literal on
// stack is handed to the gate, either as an argument to a gate entry point
// or as the Apply field of a mutation literal.
func insideGate(stack []ast.Node) bool {
	for i := len(stack) - 1; i > 0; i-- {
		if _, ok := stack[i].(*ast.FuncLit); !ok {
			continue
		}
		switch parent := stack[i-1].(type) {
		case *ast.CallExpr:
			if id, ok := parent.Fun.(*ast.Ident); ok && gateEntryPoints[id.Name] {
				return true
			}
		case *ast.KeyValueExpr:
			if id, ok := parent.Key.(*ast.Ident); ok && id.Name == "Apply" {
				return true
			}
		}
		return false
	}
	return false
}

// TestMutationsOnlyThroughGate enforces architectural constraint 1 in
// CLAUDE.md: no code outside the gate may call a mutating client method.
// It derives the call sites from the source, so a new one cannot escape it.
func TestMutationsOnlyThroughGate(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				if call, ok := n.(*ast.CallExpr); ok && isClientMutation(call) {
					found++
					if !insideGate(stack) {
						t.Errorf("%s: mutating client call outside the gate; wrap it in applyMutation or tryFixAction",
							fset.Position(call.Pos()))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	// A guard that matched nothing would pass forever.
	if found == 0 {
		t.Fatal("found no mutating client calls at all; the matcher is broken")
	}
	t.Logf("checked %d mutating client call sites", found)
}

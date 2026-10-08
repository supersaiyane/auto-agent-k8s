package kube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// untestedDetectors was the list of detectors written before detectors
// needed tests. PLAN-002 phase 12 (ISS-039) tested every one of them, so it
// is empty and must stay empty: a new detector needs a test from day one.
var untestedDetectors = map[string]bool{}

// PLAN-002 10.17: every exported Check* detector is called by a test.
func TestEveryDetectorHasATest(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	detectors := map[string]bool{}
	called := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "_test.go") {
			ast.Inspect(f, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok {
						called[id.Name] = true
					}
				}
				return true
			})
			continue
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Check") {
				detectors[fd.Name.Name] = true
			}
		}
	}
	if len(detectors) == 0 {
		t.Fatal("found no detectors; the scanner is broken")
	}
	var missing, stale []string
	for d := range detectors {
		switch {
		case !called[d] && !untestedDetectors[d]:
			missing = append(missing, d)
		case called[d] && untestedDetectors[d]:
			stale = append(stale, d)
		}
	}
	for d := range untestedDetectors {
		if !detectors[d] {
			stale = append(stale, d+" (no longer exists)")
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("detectors without a test: %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("remove from untestedDetectors, they are tested or gone now: %v", stale)
	}
}

// ISS-039, PLAN-002 E.1: every function the leader runs on a timer (the
// check table in cmd/auto-agent/run.go) is called by a test, not only the
// ones named Check*.
func TestEveryLeaderCheckHasATest(t *testing.T) {
	fset := token.NewFileSet()
	run, err := parser.ParseFile(fset, "../../cmd/auto-agent/run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, d := range run.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "leaderChecks" {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "kube" {
					listed[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	if len(listed) < 10 {
		t.Fatalf("found %d leader checks; the scanner is broken", len(listed))
	}
	called := map[string]bool{}
	tests, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range tests {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
			}
			return true
		})
	}
	var missing []string
	for name := range listed {
		if !called[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("leader checks without a test: %v", missing)
	}
	t.Logf("%d leader checks, each called by a test", len(listed))
}

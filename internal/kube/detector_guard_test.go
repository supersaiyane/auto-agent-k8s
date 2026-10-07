package kube

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// untestedDetectors were written before detectors needed tests. PLAN-002
// phase 12 (ISS-039) tests each one and removes it from this list; the list
// only shrinks, because the test below fails on a stale entry too.
var untestedDetectors = map[string]bool{
	"CheckAnomalies": true, "CheckCronJobMissed": true, "CheckDaemonSetMissing": true,
	"CheckDeadlineExceeded": true, "CheckDeploymentPaused": true, "CheckEphemeralStorageFull": true,
	"CheckFailedJobs": true, "CheckNetworkIssues": true, "CheckNodeExtended": true,
	"CheckNodeHealth": true, "CheckPendingPVCs": true, "CheckRBACDenied": true,
	"CheckReplicaSetFailure": true, "CheckResourceQuotas": true, "CheckSecurityIssues": true,
	"CheckServiceEndpoints": true, "CheckStatefulSetStuck": true, "CheckStorageIssues": true,
	"CheckStuckRollouts": true, "CheckWebhookBlocking": true,
}

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

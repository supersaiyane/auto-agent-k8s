package kube

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// funcNode is one function in the agent's source: the typed client calls in
// its body and the names it refers to (calls, method values, function values).
type funcNode struct {
	perms map[perm]bool
	refs  []string // "pkg.Name", or "*.Name" for a method on any type
}

// funcGraph keys are "pkg.Func" or "pkg.Type.Method".
type funcGraph map[string]*funcNode

const modulePath = "github.com/supersaiyane/auto-agent-k8s/"

// buildFuncGraph parses internal/ and cmd/ without type information. A
// method reference resolves to every method of that name, so reachability
// over-approximates: a role may be shown to need a permission it never uses,
// never the other way round.
func buildFuncGraph(t *testing.T) funcGraph {
	t.Helper()
	g := funcGraph{}
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
			pkg := f.Name.Name
			imports := map[string]string{} // local name -> package name, for our packages
			for _, im := range f.Imports {
				p, err := strconv.Unquote(im.Path.Value)
				if err != nil || !strings.HasPrefix(p, modulePath) {
					continue
				}
				name := p[strings.LastIndex(p, "/")+1:]
				local := name
				if im.Name != nil {
					local = im.Name.Name
				}
				imports[local] = name
			}
			for _, d := range f.Decls {
				// Package-level variables count as nodes too: a route table of
				// method values is how the API handlers are reached.
				if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.VAR {
					for _, spec := range gd.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							if i < len(vs.Values) {
								g[pkg+"."+name.Name] = collect(t, fset, vs.Values[i], pkg, imports)
							}
						}
					}
					continue
				}
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				key := pkg + "." + fd.Name.Name
				if fd.Recv != nil && len(fd.Recv.List) == 1 {
					key = pkg + "." + recvName(fd.Recv.List[0].Type) + "." + fd.Name.Name
				}
				g[key] = collect(t, fset, fd.Body, pkg, imports)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return g
}

// collect records the typed client calls and references under one node.
func collect(t *testing.T, fset *token.FileSet, root ast.Node, pkg string, imports map[string]string) *funcNode {
	n := &funcNode{perms: map[perm]bool{}}
	ast.Inspect(root, func(x ast.Node) bool {
		switch e := x.(type) {
		case *ast.CallExpr:
			if p, ok, unknown := typedCallPerm(e); ok {
				n.perms[p] = true
			} else if unknown != "" {
				t.Errorf("%s: %s has no RBAC mapping; add it to typedAccessors", fset.Position(e.Pos()), unknown)
			}
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok {
				if other, ok := imports[id.Name]; ok {
					n.refs = append(n.refs, other+"."+e.Sel.Name)
					return true
				}
			}
			n.refs = append(n.refs, "*."+e.Sel.Name)
		case *ast.Ident:
			n.refs = append(n.refs, pkg+"."+e.Name)
		}
		return true
	})
	return n
}

func recvName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.StarExpr:
		return recvName(v.X)
	case *ast.IndexExpr: // generic receiver
		return recvName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return "?"
}

// reach returns every permission the code reachable from roots uses.
func (g funcGraph) reach(t *testing.T, roots ...string) map[perm]bool {
	t.Helper()
	byMethod := map[string][]string{}
	for k := range g {
		if parts := strings.Split(k, "."); len(parts) == 3 {
			byMethod[parts[2]] = append(byMethod[parts[2]], k)
		}
	}
	seen := map[string]bool{}
	need := map[perm]bool{}
	var visit func(string)
	visit = func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		n := g[k]
		for p := range n.perms {
			need[p] = true
		}
		for _, r := range n.refs {
			if strings.HasPrefix(r, "*.") {
				for _, m := range byMethod[r[2:]] {
					visit(m)
				}
				continue
			}
			if _, ok := g[r]; ok {
				visit(r)
			}
		}
	}
	for _, r := range roots {
		if _, ok := g[r]; !ok {
			t.Fatalf("root %s not found; the role's entry points moved", r)
		}
		visit(r)
	}
	return need
}

// Entry points of each role (ADR-001), as run() starts them.
var (
	nodeRoots = []string{"kube.StartWatchers", "kube.StartLogRetention", "policy.HotReloader.Start", "crd.StartController"}
	ctrlRoots = []string{"main.leaderLoops", "main.newLeaderTarget", "main.newPeerResolver", "leader.Start", "httpapi.NewServer",
		"policy.HotReloader.Start", "crd.StartController"}
)

// Library permissions per role: informers, leader election and the CRD
// controller, which the typed-call scan cannot see.
var (
	nodeLibraryPerms = []perm{
		{"", "pods", "list"}, {"", "pods", "watch"}, // pod informer, own node
		{"", "nodes", "list"}, {"", "nodes", "watch"}, // node informer, own node
		{"", "configmaps", "list"}, {"", "configmaps", "watch"}, // policy hot reload
		{"autoagent.io", "autoremediationpolicies", "get"}, {"autoagent.io", "autoremediationpolicies", "list"},
		{"autoagent.io", "autoremediationpolicies", "watch"},
	}
	ctrlLibraryPerms = []perm{
		{"", "configmaps", "list"}, {"", "configmaps", "watch"},
		{"autoagent.io", "autoremediationpolicies", "get"}, {"autoagent.io", "autoremediationpolicies", "list"},
		{"autoagent.io", "autoremediationpolicies", "watch"},
		{"coordination.k8s.io", "leases", "get"}, {"coordination.k8s.io", "leases", "create"},
		{"coordination.k8s.io", "leases", "update"},
	}
)

type renderedBinding struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	RoleRef struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
	Subjects []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"subjects"`
}

// grantsByAccount renders the chart and returns, per ServiceAccount, every
// permission the roles bound to it grant.
func grantsByAccount(t *testing.T, args ...string) map[string]map[perm]bool {
	t.Helper()
	roles := renderRoles(t, args...)
	var bindings []renderedBinding
	for _, raw := range renderKinds(t, map[string]bool{"ClusterRoleBinding": true, "RoleBinding": true}, args...) {
		var b renderedBinding
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("binding: %v", err)
		}
		bindings = append(bindings, b)
	}
	out := map[string]map[perm]bool{}
	for _, b := range bindings {
		for _, r := range roles {
			same := r.Kind == b.RoleRef.Kind && r.Metadata.Name == b.RoleRef.Name
			if r.Kind == "Role" {
				same = same && r.Metadata.Namespace == b.Metadata.Namespace
			}
			if !same {
				continue
			}
			for _, s := range b.Subjects {
				if s.Kind != "ServiceAccount" {
					continue
				}
				if out[s.Name] == nil {
					out[s.Name] = map[perm]bool{}
				}
				for _, p := range grantsOf(r) {
					out[s.Name][p] = true
				}
			}
		}
	}
	return out
}

// ADR-001, constraint 7: each ServiceAccount grants exactly what the code its
// role runs uses, found by walking the call graph from the role's entry points.
// Checked with writes in one ceiling namespace and with fix-anywhere (ADR-002).
func TestRBAC_EachRoleMatchesItsCode(t *testing.T) {
	g := buildFuncGraph(t)
	for _, args := range [][]string{
		{"--set", "agent.fixNamespaces={default}"},
		{"--set", "rbac.fixAnywhere=true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) { eachRoleMatchesItsCode(t, g, grantsByAccount(t, args...)) })
	}
}

func eachRoleMatchesItsCode(t *testing.T, g funcGraph, granted map[string]map[perm]bool) {
	for _, role := range []struct {
		account string
		roots   []string
		library []perm
	}{
		{"auto-agent-node", nodeRoots, nodeLibraryPerms},
		{"auto-agent", ctrlRoots, ctrlLibraryPerms},
	} {
		need := g.reach(t, role.roots...)
		for _, p := range role.library {
			need[p] = true
		}
		have := granted[role.account]
		if len(have) == 0 {
			t.Fatalf("%s: nothing bound to this account", role.account)
		}
		var missing, extra []string
		for p := range need {
			if !have[p] && optionalPerms[p] == "" {
				missing = append(missing, p.String())
			}
		}
		for p := range have {
			if !need[p] {
				extra = append(extra, p.String())
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			t.Errorf("%s: code uses permissions the chart does not grant: %v", role.account, missing)
		}
		if len(extra) > 0 {
			t.Errorf("%s: chart grants permissions this role's code never uses: %v", role.account, extra)
		}
	}
}

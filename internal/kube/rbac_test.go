package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// perm is one RBAC grant: API group, resource, verb.
type perm struct{ group, resource, verb string }

func (p perm) String() string { return p.group + "/" + p.resource + ":" + p.verb }

// typedAccessors maps typed client accessors to their API group and resource.
// An accessor missing here fails TestRBAC_ChartMatchesCode, so a new API
// call cannot be added without deciding its permission.
var typedAccessors = map[string]perm{
	"Pods": {"", "pods", ""}, "Nodes": {"", "nodes", ""}, "Events": {"", "events", ""},
	"Namespaces": {"", "namespaces", ""}, "ConfigMaps": {"", "configmaps", ""},
	"Endpoints": {"", "endpoints", ""}, "LimitRanges": {"", "limitranges", ""},
	"EndpointSlices":         {"discovery.k8s.io", "endpointslices", ""},
	"PersistentVolumeClaims": {"", "persistentvolumeclaims", ""}, "ResourceQuotas": {"", "resourcequotas", ""},
	"Secrets": {"", "secrets", ""}, "Services": {"", "services", ""},
	"Deployments": {"apps", "deployments", ""}, "ReplicaSets": {"apps", "replicasets", ""},
	"StatefulSets": {"apps", "statefulsets", ""}, "DaemonSets": {"apps", "daemonsets", ""},
	"Jobs": {"batch", "jobs", ""}, "CronJobs": {"batch", "cronjobs", ""},
	"HorizontalPodAutoscalers": {"autoscaling", "horizontalpodautoscalers", ""},
	"Ingresses":                {"networking.k8s.io", "ingresses", ""}, "StorageClasses": {"storage.k8s.io", "storageclasses", ""},
	"Evictions": {"", "pods/eviction", ""}, "PodDisruptionBudgets": {"policy", "poddisruptionbudgets", ""},
}

var clientVerbs = map[string]string{
	"Get": "get", "List": "list", "Watch": "watch", "Create": "create", "Update": "update",
	"Patch": "patch", "Delete": "delete", "Evict": "create", "GetLogs": "get",
}

// libraryPerms are used through informers, leader election and the dynamic
// client, which the typed-call scan cannot see.
var libraryPerms = []perm{
	{"", "pods", "list"}, {"", "pods", "watch"}, // pod informer (watcher.go)
	{"", "nodes", "list"}, {"", "nodes", "watch"}, // node informer (watcher.go)
	{"", "configmaps", "list"}, {"", "configmaps", "watch"}, // policy hot reload (reload.go)
	{"autoagent.io", "autoremediationpolicies", "get"}, // CRD controller (crd/controller.go)
	{"autoagent.io", "autoremediationpolicies", "list"},
	{"autoagent.io", "autoremediationpolicies", "watch"},
	{"coordination.k8s.io", "leases", "get"}, // leader election (leader.go)
	{"coordination.k8s.io", "leases", "create"},
	{"coordination.k8s.io", "leases", "update"},
}

// optionalPerms are used by code that tolerates their absence and are granted
// only when a chart value enables them.
var optionalPerms = map[perm]string{
	{"", "secrets", "list"}: "rbac.readTLSSecrets (certificate expiry check)",
}

// workloadWrites must be granted only by namespaced Roles in allowlisted
// namespaces, never cluster-wide (CLAUDE.md constraints 4 and 7).
var workloadWrites = map[perm]bool{
	{"", "pods", "delete"}: true, {"", "pods/eviction", "create"}: true,
	{"apps", "deployments", "patch"}: true, {"apps", "deployments", "update"}: true,
	{"batch", "jobs", "delete"}: true,
}

// codePerms scans non-test Go source for typed client calls.
func codePerms(t *testing.T) map[perm]bool {
	t.Helper()
	need := map[perm]bool{}
	for _, p := range libraryPerms {
		need[p] = true
	}
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
				if !ok {
					return true
				}
				if p, ok, unknown := typedCallPerm(call); ok {
					need[p] = true
				} else if unknown != "" {
					t.Errorf("%s: %s has no RBAC mapping; add it to typedAccessors", fset.Position(call.Pos()), unknown)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return need
}

// typedCallPerm maps a typed client call such as
// kc.CoreV1().Pods(ns).List(...) to its permission. unknown names an
// accessor missing from typedAccessors.
func typedCallPerm(call *ast.CallExpr) (p perm, ok bool, unknown string) {
	verbSel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || clientVerbs[verbSel.Sel.Name] == "" {
		return perm{}, false, ""
	}
	resCall, ok := verbSel.X.(*ast.CallExpr)
	if !ok {
		return perm{}, false, ""
	}
	resSel, ok := resCall.Fun.(*ast.SelectorExpr)
	if !ok {
		return perm{}, false, ""
	}
	gvCall, ok := resSel.X.(*ast.CallExpr)
	if !ok {
		return perm{}, false, ""
	}
	gvSel, ok := gvCall.Fun.(*ast.SelectorExpr)
	if !ok || !groupVersion.MatchString(gvSel.Sel.Name) {
		return perm{}, false, ""
	}
	p, known := typedAccessors[resSel.Sel.Name]
	if !known {
		return perm{}, false, gvSel.Sel.Name + "()." + resSel.Sel.Name
	}
	p.verb = clientVerbs[verbSel.Sel.Name]
	if verbSel.Sel.Name == "GetLogs" {
		p.resource = "pods/log"
	}
	return p, true, ""
}

type renderedRole struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules []rbacv1.PolicyRule `json:"rules"`
}

// renderKinds renders the chart and returns the JSON of every object whose
// kind is in kinds.
func renderKinds(t *testing.T, kinds map[string]bool, args ...string) [][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed; RBAC check needs `helm template`")
	}
	out, err := exec.Command("helm", append([]string{"template", "auto-agent", "../../charts/auto-agent"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	var objs [][]byte
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var raw map[string]any
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode chart output: %v", err)
		}
		kind, _ := raw["kind"].(string) // an object without a kind is skipped
		if !kinds[kind] {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("encode %s: %v", kind, err)
		}
		objs = append(objs, b)
	}
	return objs
}

func renderRoles(t *testing.T, args ...string) []renderedRole {
	t.Helper()
	var roles []renderedRole
	for _, b := range renderKinds(t, map[string]bool{"ClusterRole": true, "Role": true}, args...) {
		var r renderedRole
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatalf("role: %v", err)
		}
		roles = append(roles, r)
	}
	return roles
}

func grantsOf(r renderedRole) []perm {
	var ps []perm
	for _, rule := range r.Rules {
		for _, g := range rule.APIGroups {
			for _, res := range rule.Resources {
				for _, v := range rule.Verbs {
					ps = append(ps, perm{g, res, v})
				}
			}
		}
	}
	return ps
}

// ISS-009, constraint 7: the chart grants exactly what the code uses, with
// writes in one ceiling namespace and with fix-anywhere (ADR-002) alike.
func TestRBAC_ChartMatchesCode(t *testing.T) {
	for _, args := range [][]string{
		{"--set", "agent.fixNamespaces={default}"},
		{"--set", "rbac.fixAnywhere=true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) { chartMatchesCode(t, args) })
	}
}

func chartMatchesCode(t *testing.T, args []string) {
	need := codePerms(t)
	granted := map[perm]bool{}
	for _, r := range renderRoles(t, args...) {
		for _, p := range grantsOf(r) {
			granted[p] = true
		}
	}

	var missing, extra []string
	for p := range need {
		if !granted[p] && optionalPerms[p] == "" {
			missing = append(missing, p.String())
		}
	}
	for p := range granted {
		if !need[p] {
			extra = append(extra, p.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("code uses permissions the chart does not grant: %v", missing)
	}
	if len(extra) > 0 {
		t.Errorf("chart grants permissions no code uses: %v", extra)
	}
	for p, value := range optionalPerms {
		if granted[p] {
			t.Errorf("%s is granted by default; it should need %s", p, value)
		}
	}
}

// Constraints 4 and 7: workload writes only in allowlisted namespaces, and
// the lease update is pinned to the agent's own lease.
func TestRBAC_WritesAreNamespacedAndLeasePinned(t *testing.T) {
	roles := renderRoles(t, "--set", "agent.fixNamespaces={default,prod}")
	writeNS := map[string]bool{}
	for _, r := range roles {
		for _, rule := range r.Rules {
			for _, g := range rule.APIGroups {
				for _, res := range rule.Resources {
					for _, v := range rule.Verbs {
						p := perm{g, res, v}
						if workloadWrites[p] {
							if r.Kind == "ClusterRole" {
								t.Errorf("ClusterRole %s grants %s cluster-wide", r.Metadata.Name, p)
							} else {
								writeNS[r.Metadata.Namespace] = true
							}
						}
						if g == "coordination.k8s.io" && v == "update" {
							if r.Kind != "Role" || len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "auto-agent-leader" {
								t.Errorf("%s %s: lease update must be a Role pinned to auto-agent-leader, got %v",
									r.Kind, r.Metadata.Name, rule.ResourceNames)
							}
						}
					}
				}
			}
		}
	}
	for _, ns := range []string{"default", "prod"} {
		if !writeNS[ns] {
			t.Errorf("no write Role in allowlisted namespace %s", ns)
		}
	}
	if len(writeNS) != 2 {
		t.Errorf("write Roles outside the allowlist: %v", writeNS)
	}
}

func TestRBAC_TLSSecretsOptIn(t *testing.T) {
	roles := renderRoles(t, "--set", "rbac.readTLSSecrets=true", "--set", "agent.fixNamespaces={default}")
	for _, r := range roles {
		for _, p := range grantsOf(r) {
			if p.resource == "secrets" && r.Kind == "Role" && r.Metadata.Namespace == "default" {
				return
			}
		}
	}
	t.Error("rbac.readTLSSecrets=true should grant secrets list in each allowlisted namespace")
}

func TestCountAPIError_ForbiddenIsCounted(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "widgets"}, "", errors.New("rbac"))
	c := obs.APIErrorsTotal.WithLabelValues("widgets", "forbidden")
	before := testutil.ToFloat64(c)
	countAPIError(forbidden, "widgets", "default")
	countAPIError(forbidden, "widgets", "default")
	if got := testutil.ToFloat64(c) - before; got != 2 {
		t.Fatalf("expected 2 forbidden reads counted, got %v", got)
	}
}

// The cert expiry check reads secrets, which the chart grants only with
// rbac.readTLSSecrets. Without the opt-in it must not even try (ISS-009).
func TestCertExpiryCheck_OnlyWhenOptedIn(t *testing.T) {
	for _, tc := range []struct {
		env       string
		wantReads bool
	}{{"", false}, {"false", false}, {"true", true}} {
		t.Run("TLS_CERT_CHECK="+tc.env, func(t *testing.T) {
			deps, kc := newHandlerTestDeps(t)
			deps.TLSCertCheck = tc.env == "true" // parsing is tested in internal/config
			reads := 0
			kc.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				reads++
				return false, nil, nil
			})
			checkCertExpiry(context.Background(), deps)
			if (reads > 0) != tc.wantReads {
				t.Fatalf("secret list calls = %d, want reads=%v", reads, tc.wantReads)
			}
		})
	}
}

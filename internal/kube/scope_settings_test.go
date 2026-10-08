package kube

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func scopeDeps(t *testing.T) (*Deps, *fake.Clientset, *events.Recorder) {
	t.Helper()
	deps, _ := newHandlerTestDeps(t)
	kc := fake.NewSimpleClientset()
	rec := events.NewRecorder(20)
	deps.Client, deps.Recorder = kc, rec
	deps.Policies = policy.Static(policy.Load(func(k string) string {
		return map[string]string{"FIX_NAMESPACES": "a", "FIX_CEILING": "a,b"}[k]
	}))
	return deps, kc, rec
}

func scopeData(t *testing.T, kc *fake.Clientset) (map[string]string, bool) {
	t.Helper()
	cm, err := kc.CoreV1().ConfigMaps("agent").Get(context.Background(), policy.ScopeConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return cm.Data, true
}

// ADR-002: the choice is created on first save, patched after, cleared by
// nil, and every attempt is an audit event naming before, after and origin.
func TestSaveFixScope(t *testing.T) {
	ctx := context.Background()
	deps, kc, rec := scopeDeps(t)

	if err := SaveFixScope(ctx, deps, "agent", nil, "test"); err != nil {
		t.Fatalf("clearing when nothing is stored: %v", err)
	}
	if _, ok := scopeData(t, kc); ok {
		t.Fatal("clearing never creates the ConfigMap")
	}
	if err := SaveFixScope(ctx, deps, "agent", []string{"b"}, "10.0.0.7"); err != nil {
		t.Fatal(err)
	}
	if d, _ := scopeData(t, kc); d[policy.ScopeKey] != "b" {
		t.Fatalf("created: %v", d)
	}
	if err := SaveFixScope(ctx, deps, "agent", []string{}, "test"); err != nil {
		t.Fatal(err)
	}
	if d, _ := scopeData(t, kc); d[policy.ScopeKey] != "" || policy.ScopeChoice(d) == nil {
		t.Fatalf("an empty choice is stored as a present, empty key: %v", d)
	}
	if err := SaveFixScope(ctx, deps, "agent", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if d, ok := scopeData(t, kc); !ok || policy.ScopeChoice(d) != nil {
		t.Fatalf("clearing removes the key: %v", d)
	}

	err := SaveFixScope(ctx, deps, "agent", []string{"a", "c"}, "test")
	if !errors.Is(err, ErrOutsideCeiling) || !strings.Contains(err.Error(), "c") {
		t.Fatalf("outside the ceiling: %v", err)
	}
	if d, _ := scopeData(t, kc); policy.ScopeChoice(d) != nil {
		t.Fatal("a refused choice writes nothing")
	}

	kc.PrependReactor("patch", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, policy.ScopeConfigMap, errors.New("rbac"))
	})
	if err := SaveFixScope(ctx, deps, "agent", []string{"a"}, "test"); !apierrors.IsForbidden(err) {
		t.Fatalf("an API error is returned: %v", err)
	}

	var results []string
	for _, e := range rec.Recent(20) {
		if e.Type != events.Audit || e.Action != "set_fix_scope" {
			t.Fatalf("unexpected event %+v", e)
		}
		results = append([]string{e.Result}, results...)
		if e.Result == "success" && strings.Contains(e.Message, "-> [b]") && !strings.Contains(e.Message, "from 10.0.0.7") {
			t.Fatalf("audit names the origin: %s", e.Message)
		}
	}
	if got := strings.Join(results, ","); got != "success,success,success,success,blocked,failed" {
		t.Fatalf("audit results %s", got)
	}
}

// ISS-064: a typed client kept in a variable would hide its calls from the
// mutation guard and the RBAC scan; the guard recognizes both forms.
func TestStoresTypedClient(t *testing.T) {
	for src, want := range map[string]bool{
		`c := kc.CoreV1()`:                           true,
		`c := deps.Client.CoreV1().ConfigMaps(ns)`:   true,
		`c := kc.AppsV1beta1().Deployments("x")`:     true,
		`_, err := kc.CoreV1().Pods(ns).Get(ctx, n)`: false,
		`x := strings.Split(a, b)`:                   false,
	} {
		f := parseStmt(t, src)
		got := false
		for _, rhs := range assignedValues(f) {
			got = got || storesTypedClient(rhs)
		}
		if got != want {
			t.Errorf("%s: stores client %v, want %v", src, got, want)
		}
	}
}

// parseStmt parses one Go statement.
func parseStmt(t *testing.T, src string) ast.Node {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package p\nfunc f() {\n"+src+"\n}", 0)
	if err != nil {
		t.Fatal(err)
	}
	return f.Decls[0].(*ast.FuncDecl).Body.List[0]
}

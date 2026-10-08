package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

type savedScope struct {
	names []string
	from  string
	calls int
	err   error
}

func scopeServer(t *testing.T, env map[string]string) (*Server, *savedScope, *policy.Policy) {
	t.Helper()
	pol := policy.Load(func(k string) string { return env[k] })
	kc := fake.NewSimpleClientset()
	for _, n := range []string{"kube-system", "a", "b", "c"} {
		if _, err := kc.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	saved := &savedScope{}
	s := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, kc, Options{
		DashboardToken: "dash",
		Scope: ScopeOptions{
			Policy: func() *policy.Policy { return pol },
			Save: func(_ context.Context, names []string, from string) error {
				saved.calls++
				saved.names, saved.from = names, from
				if saved.err != nil {
					return saved.err
				}
				for _, n := range names {
					if !pol.InCeiling(n) {
						return fmt.Errorf("%s: %w", n, kube.ErrOutsideCeiling)
					}
				}
				return nil
			},
		},
	})
	return s, saved, pol
}

func scopeCall(s *Server, method, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/scope", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer dash")
	r.RemoteAddr = "10.1.2.3:5555"
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	return w
}

// ADR-002: the Settings tab sees every watched namespace with what it may
// enable, and nothing outside the watch scope.
func TestScopeAPI_Get(t *testing.T) {
	s, _, _ := scopeServer(t, map[string]string{"FIX_NAMESPACES": "a", "FIX_CEILING": "a,b"})
	w := scopeCall(s, http.MethodGet, "")
	var v scopeView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	got := []string{}
	for _, n := range v.Namespaces {
		got = append(got, fmt.Sprintf("%s:%v:%v", n.Name, n.InCeiling, n.Fixable))
	}
	if strings.Join(got, " ") != "a:true:true b:true:false c:false:false" || !v.WatchAll || v.FixAnywhere ||
		strings.Join(v.FixScope, ",") != "a" || strings.Join(v.HelmFix, ",") != "a" || v.Choice != nil {
		t.Fatalf("view: %+v", v)
	}

	s, _, _ = scopeServer(t, map[string]string{"WATCH_NAMESPACES": "b"})
	if body := scopeCall(s, http.MethodGet, "").Body.String(); strings.Contains(body, `"a"`) || !strings.Contains(body, `"b"`) {
		t.Fatalf("an explicit watch list is all the tab shows: %s", body)
	}
}

func TestScopeAPI_Put(t *testing.T) {
	s, saved, _ := scopeServer(t, map[string]string{"FIX_NAMESPACES": "a", "FIX_CEILING": "a,b"})
	for _, tc := range []struct {
		name, body string
		want       int
		wantSaved  int
	}{
		{"bad json", `{`, http.StatusBadRequest, 0},
		{"missing list", `{"confirm":[]}`, http.StatusBadRequest, 0},
		{"enabling without typing the name", `{"fixNamespaces":["a","b"],"confirm":[]}`, http.StatusBadRequest, 0},
		{"enabling outside the ceiling", `{"fixNamespaces":["c"],"confirm":["c"]}`, http.StatusBadRequest, 1},
		{"narrowing needs no confirmation", `{"fixNamespaces":[],"confirm":[]}`, http.StatusNoContent, 2},
		{"enabling with the name typed", `{"fixNamespaces":["b","a"],"confirm":["b"]}`, http.StatusNoContent, 3},
	} {
		w := scopeCall(s, http.MethodPut, tc.body)
		if w.Code != tc.want || saved.calls != tc.wantSaved {
			t.Errorf("%s: %d (%s), saved %d times", tc.name, w.Code, strings.TrimSpace(w.Body.String()), saved.calls)
		}
	}
	if strings.Join(saved.names, ",") != "a,b" || saved.from != "dashboard via 10.1.2.3" {
		t.Fatalf("saved %v from %q", saved.names, saved.from)
	}
	if w := scopeCall(s, http.MethodPut, `{"fixNamespaces":["c"],"confirm":["c"]}`); !strings.Contains(w.Body.String(), "agent.fixCeiling") {
		t.Fatalf("the refusal says where to change the ceiling: %s", w.Body.String())
	}
	saved.err = errors.New("api down")
	if w := scopeCall(s, http.MethodPut, `{"fixNamespaces":["a"],"confirm":[]}`); w.Code != http.StatusBadGateway {
		t.Fatalf("a failed save: %d", w.Code)
	}
}

func TestScopeAPI_DeleteMethodsAndDisabled(t *testing.T) {
	s, saved, _ := scopeServer(t, map[string]string{"FIX_NAMESPACES": "a"})
	if w := scopeCall(s, http.MethodDelete, ""); w.Code != http.StatusNoContent || saved.calls != 1 || saved.names != nil {
		t.Fatalf("delete returns to the Helm list: %d %v", w.Code, saved.names)
	}
	if w := scopeCall(s, http.MethodPost, "{}"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", w.Code)
	}
	plain := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{DashboardToken: "dash"})
	if w := scopeCall(plain, http.MethodGet, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without scope options: %d", w.Code)
	}
}

func TestScopeAPI_OriginThroughStandby(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "/api/scope", nil)
	r.RemoteAddr = "10.0.0.9:1"
	r.Header.Set("X-Forwarded-For", "192.0.2.4, 10.0.0.9")
	if got := requestOrigin(r); got != "dashboard via 10.0.0.9" {
		t.Fatalf("an unproxied request ignores X-Forwarded-For: %s", got)
	}
	r.Header.Set(proxiedHeader, "1")
	if got := requestOrigin(r); got != "dashboard via 192.0.2.4" {
		t.Fatalf("a proxied request names the client: %s", got)
	}
	r.RemoteAddr = "garbage"
	r.Header.Del(proxiedHeader)
	if got := requestOrigin(r); got != "dashboard via garbage" {
		t.Fatalf("an unparsable address is kept: %s", got)
	}
}

// ADR-002: /api/status reports the live mode and fix-anywhere.
func TestStatus_ReportsScope(t *testing.T) {
	s, _, _ := scopeServer(t, map[string]string{"FIX_ANYWHERE": "true", "FIX_NAMESPACES": "a", "AUTO_MODE": "fix"})
	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	r.Header.Set("Authorization", "Bearer dash")
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{`"fixAnywhere":true`, `"fixScope":["a"]`, `"mode":"fix"`, `"watchAll":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status lacks %s: %s", want, body)
		}
	}
}

type fakeReloads []kube.ReloadRecord

func (f fakeReloads) Records() []kube.ReloadRecord { return f }

// PLAN-002 A3.3: the Reloads tab shows reloads in watched namespaces only.
func TestReloadsAPI(t *testing.T) {
	recs := fakeReloads{{ID: 2, Namespace: "default", Object: "configmap/app", Keys: []string{"level"},
		Workloads: []kube.ReloadOutcome{{Workload: "deployment/api", Result: "simulated"}}},
		{ID: 1, Namespace: "hidden-ns", Object: "configmap/x"}}
	s := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{
		DashboardToken: "dash", AllowNamespace: func(ns string) bool { return ns == "default" }, Extended: ExtendedDeps{Reloads: recs}})
	r := httptest.NewRequest(http.MethodGet, "/api/reloads", nil)
	r.Header.Set("Authorization", "Bearer dash")
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `"configmap/app"`) || strings.Contains(body, "hidden-ns") || !strings.Contains(body, `"simulated"`) {
		t.Fatalf("reloads: %d %s", w.Code, body)
	}
	empty := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{DashboardToken: "dash"})
	w = httptest.NewRecorder()
	empty.srv.Handler.ServeHTTP(w, r)
	if strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("reload off: %s", w.Body.String())
	}
}

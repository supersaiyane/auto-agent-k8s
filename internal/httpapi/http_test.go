package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
)

func newTestServer(t *testing.T, token string) *Server {
	t.Helper()
	return NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{DashboardToken: token})
}

func get(s *Server, path, auth string) int {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return rec.Code
}

// ISS-005: every /api/ route needs the bearer token. The routes are taken
// from the server's own table so a new route cannot escape the check.
func TestAPI_EveryRouteRequiresToken(t *testing.T) {
	s := newTestServer(t, "s3cret")
	for _, path := range apiRoutes() {
		if path == slackActionsPath {
			continue // authenticated by Slack signature instead (ISS-006)
		}
		if code := get(s, path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s without token: got %d, want 401", path, code)
		}
		if code := get(s, path, "Bearer wrong"); code != http.StatusUnauthorized {
			t.Errorf("%s with wrong token: got %d, want 401", path, code)
		}
		if code := get(s, path, "Bearer s3cret"); code == http.StatusUnauthorized {
			t.Errorf("%s with the right token: got 401", path)
		}
	}
	if len(apiRoutes()) < 10 {
		t.Fatalf("route table looks empty: %v", apiRoutes())
	}
}

func TestAPI_NoTokenConfiguredFailsClosed(t *testing.T) {
	s := newTestServer(t, "")
	if code := get(s, "/api/status", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 when DASHBOARD_TOKEN is unset", code)
	}
}

func TestAPI_ProbesAndMetricsStayOpen(t *testing.T) {
	s := newTestServer(t, "s3cret")
	for _, path := range []string{"/healthz", "/metrics"} {
		if code := get(s, path, ""); code != http.StatusOK {
			t.Errorf("%s: got %d, want 200 without a token", path, code)
		}
	}
}

// Constraint 4: the kubectl endpoint reads only allowlisted namespaces.
func TestKubectl_NamespaceAllowlist(t *testing.T) {
	ctx := context.Background()
	kc := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	allow := func(ns string) bool { return ns == "default" }

	for _, tc := range []struct {
		cmd     string
		allowed bool
	}{
		{"get pods", true},
		{"get pods -n default", true},
		{"get nodes", true},
		{"get namespaces", true},
		{"describe node node-1", true},
		{"version", true},
		{"get pods -n kube-system", false},
		{"get pods -A", false},
		{"get all --all-namespaces", false},
		{"logs etcd -n kube-system", false},
		{"describe pod etcd -n kube-system", false},
		{"top pods -n kube-system", false},
	} {
		_, err := executeKubectl(ctx, kc, tc.cmd, allow)
		denied := err != nil && (strings.Contains(err.Error(), "outside the watch scope") || strings.Contains(err.Error(), "not supported yet"))
		if tc.allowed == denied {
			t.Errorf("%q: allowed=%v, err=%v", tc.cmd, tc.allowed, err)
		}
	}

	if _, err := executeKubectl(ctx, kc, "get pods", nil); err == nil {
		t.Error("nil allowlist must deny namespaced reads")
	}
}

// ISS-028, constraint 4: no dashboard route returns data from a namespace
// outside the allowlist. The routes come from apiRouteTable, so a new route
// is covered without editing this test.
func TestAPI_NoRouteLeaksNonAllowlistedNamespaces(t *testing.T) {
	kc := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "hidden-ns"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: corev1.PodSpec{NodeName: "node-1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "hidden-pod-sentinel", Namespace: "hidden-ns"},
			Spec: corev1.PodSpec{NodeName: "node-1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "hidden-deploy-sentinel", Namespace: "hidden-ns"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "hidden-ns"},
			Message: "hidden-event-sentinel", InvolvedObject: corev1.ObjectReference{Name: "hidden-pod-sentinel"}},
	)
	s := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, kc, Options{DashboardToken: "s3cret"})
	s.SetNamespaceFilter(func(ns string) bool { return ns == "default" })

	paths := []string{"/api/namespace/hidden-ns", "/api/resources/hidden-ns", "/api/k8s-events?namespace=hidden-ns"}
	for _, p := range apiRoutes() {
		if p != slackActionsPath && p != "/api/kubectl" && !strings.HasSuffix(p, "/") {
			paths = append(paths, p)
		}
	}
	for _, p := range paths {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer s3cret")
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, req)
		if strings.Contains(rec.Body.String(), "sentinel") || strings.Contains(rec.Body.String(), "hidden-ns") {
			t.Errorf("%s leaked a non-allowlisted namespace (status %d): %.300s", p, rec.Code, rec.Body.String())
		}
	}
}

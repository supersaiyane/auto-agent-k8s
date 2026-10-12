package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/httpx/httpxtest"
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
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
	)
	e := &termEnv{kc: kc, allowNS: func(ns string) bool { return ns == "default" }}
	for _, tc := range []struct {
		cmd     string
		allowed bool
	}{
		{"get pods", true},
		{"get pods -n default", true},
		{"get pods -A", true}, // ISS-063: every watched namespace
		{"get nodes", true},
		{"get namespaces", true},
		{"describe node node-1", true},
		{"get pods -n kube-system", false},
		{"logs etcd -n kube-system", false},
		{"describe pod etcd -n kube-system", false},
		{"events --for pod/etcd -n kube-system", false},
	} {
		_, err := executeKubectl(ctx, e, tc.cmd)
		denied := err != nil && strings.Contains(err.Error(), "outside the watch scope")
		if tc.allowed == denied {
			t.Errorf("%q: allowed=%v, err=%v", tc.cmd, tc.allowed, err)
		}
	}
	if out, err := executeKubectl(ctx, e, "get pods -A"); err != nil || strings.Contains(out, "etcd") || !strings.Contains(out, "web") || !strings.Contains(out, "NAMESPACE") {
		t.Fatalf("-A lists watched namespaces only: %v\n%s", err, out)
	}
	if _, err := executeKubectl(ctx, &termEnv{kc: kc}, "get pods"); err == nil {
		t.Error("a nil watch filter denies namespaced reads")
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

// ISS-079: Start binds before it returns, so a port in use is an error and
// a free port accepts connections as soon as Start returns.
func TestServer_StartBindsBeforeReturning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	taken := NewServer(ln.Addr().String(), events.NewRecorder(10), &AgentMeta{}, fake.NewSimpleClientset(), Options{HealthOnly: true})
	if err := taken.Start(); err == nil || !strings.Contains(err.Error(), "listen on") {
		t.Fatalf("port in use: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(addr, events.NewRecorder(10), &AgentMeta{}, fake.NewSimpleClientset(), Options{HealthOnly: true})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.srv.Close() })
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz right after Start: %v %v", resp, err)
	}
	resp.Body.Close()
}

func TestResourceHelpers(t *testing.T) {
	cases := []struct {
		cpuReq, cpuLim, memReq, memLim float64
		want                           string
	}{
		{0, 0, 0, 0, "no-limits"},
		{0, 1, 0, 256, "overuse"},
		{0.1, 1, 64, 128, "overuse"},
		{0.5, 1, 32, 256, "overuse"},
		{0.005, 0.5, 64, 128, "underuse"},
		{0.5, 1, 8, 256, "underuse"},
		{0.5, 1, 256, 512, "right-sized"},
	}
	for _, c := range cases {
		if got := classifyPod(c.cpuReq, c.cpuLim, c.memReq, c.memLim); got != c.want {
			t.Errorf("classifyPod(%v) = %q, want %q", c, got, c.want)
		}
	}
	if classifyContainer("-", "-", "-", "-") != "no-limits" || classifyContainer("-", "1", "-", "1Gi") != "no-requests" || classifyContainer("1", "1", "1Gi", "1Gi") != "ok" {
		t.Fatal("classifyContainer")
	}
	for eff, color := range map[string]string{"overuse": "red", "underuse": "yellow", "no-limits": "red", "right-sized": "green"} {
		if effToColor(eff) != color {
			t.Errorf("effToColor(%q)", eff)
		}
	}
	if !strings.Contains(effAdvice("no-limits", 0, 0, 0, 0), "limits") || !strings.Contains(effAdvice("overuse", 0, 1, 0, 1), "requests") ||
		!strings.Contains(effAdvice("overuse", 0.1, 1, 1, 1), "10x") || !strings.Contains(effAdvice("overuse", 0.5, 1, 1, 1), "too high") ||
		!strings.Contains(effAdvice("underuse", 0, 0, 0, 0), "throttled") || effAdvice("right-sized", 0, 0, 0, 0) != "" {
		t.Fatal("effAdvice")
	}
	if fmtCPU(0) != "-" || fmtCPU(0.25) != "250m" || fmtCPU(2) != "2.0" || fmtMem(0) != "-" || fmtMem(512) != "512Mi" || fmtMem(2048) != "2.0Gi" {
		t.Fatal("formatting")
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}}}}
	if cr, cl, mr, ml := podFullResources(pod); cr != 0.25 || cl != 1 || mr != 128 || ml != 256 {
		t.Fatalf("podFullResources: %v %v %v %v", cr, cl, mr, ml)
	}
}

func scopedServer(t *testing.T, objs ...runtime.Object) *Server {
	t.Helper()
	return NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(objs...),
		Options{DashboardToken: "tok", AllowNamespace: func(ns string) bool { return ns == "default" }})
}

func call(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestNamespaceAndResourcesViews(t *testing.T) {
	limited := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default", Labels: map[string]string{"app": "api"}},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "app", Image: "api:1", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: 2}}}}
	bare := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "b:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending}}
	one := int32(1)
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &one}}
	s := scopedServer(t, limited, bare, dep)

	rec := call(s, http.MethodGet, "/api/namespace/default", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "api-1") || !strings.Contains(rec.Body.String(), "bare") {
		t.Fatalf("namespace view: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(s, http.MethodGet, "/api/namespace/payments", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("outside the watch scope: %d", rec.Code)
	}
	if rec := call(s, http.MethodGet, "/api/namespace/", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no namespace: %d", rec.Code)
	}

	rec = call(s, http.MethodGet, "/api/resources/default", "")
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("resources view: %d %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "no-limits") || !strings.Contains(body, "250m") {
		t.Fatalf("the bare pod is flagged and requests are shown: %s", body)
	}
	if rec := call(s, http.MethodGet, "/api/resources/payments", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("resources outside the watch scope: %d", rec.Code)
	}
	if rec := call(s, http.MethodGet, "/api/resources/", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("resources without a namespace: %d", rec.Code)
	}
}

func TestKubectlEndpoint(t *testing.T) {
	s := scopedServer(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"}})
	cases := []struct{ method, body, want string }{
		{http.MethodGet, "", "POST required"},
		{http.MethodPost, "not json", "invalid request"},
		{http.MethodPost, `{"command":"get pods -n default"}`, "api-1"},
		{http.MethodPost, `{"command":"delete pod api-1"}`, "is not available"},
	}
	for _, c := range cases {
		if rec := call(s, c.method, "/api/kubectl", c.body); !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(c.want)) {
			t.Errorf("%s %q: %s", c.method, c.body, rec.Body.String())
		}
	}
}

// Kubecost and OpenCost share an allocation API; each source is named and
// a failure is returned, never a silent empty bill.
func TestFetchAllocation(t *testing.T) {
	srv := httpxtest.New(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/model/allocation"), strings.HasPrefix(r.URL.Path, "/allocation/compute"):
			httpxtest.JSON(w, 200, `{"code":200,"data":[{"default":{"totalCost":12.345},"payments":{"totalCost":7.5}}]}`)
		default:
			httpxtest.JSON(w, 500, `{}`)
		}
	})
	defer srv.Close()
	c := CostConfig{KubecostURL: "http://kubecost.test", OpenCostURL: "http://opencost.test"}
	ctx := context.Background()
	for name, fetch := range map[string]func() (*clusterCost, error){
		"kubecost": func() (*clusterCost, error) { return c.fetchKubecostData(ctx, srv.Client(time.Second)) },
		"opencost": func() (*clusterCost, error) { return c.fetchOpenCostData(ctx, srv.Client(time.Second)) },
	} {
		got, err := fetch()
		if err != nil || got.Config.Source != name || len(got.Namespaces) != 2 || got.TotalMonthly < 19.8 || got.TotalMonthly > 19.9 {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	if _, err := c.fetchAllocation(ctx, srv.Client(time.Second), "kubecost", "http://kubecost.test/other"); err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("an error status is returned: %v", err)
	}
	bad := httpxtest.New(func(w http.ResponseWriter, _ *http.Request) { httpxtest.JSON(w, 200, `not json`) })
	defer bad.Close()
	if _, err := c.fetchAllocation(ctx, bad.Client(time.Second), "opencost", "http://opencost.test/x"); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("a body that does not parse is an error: %v", err)
	}
}

func TestServerLifecycle(t *testing.T) {
	s := scopedServer(t)
	s.SetLeaderFunc(func() bool { return true })
	if !s.meta.IsLeaderFn() {
		t.Fatal("leader func")
	}
	s.SetReady()
	rec := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ready after SetReady: %d", rec.Code)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ISS-090: the dashboard files are revalidated on every load, so an upgrade
// is seen at once.
func TestUIFilesAreNotCached(t *testing.T) {
	s := scopedServer(t)
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		rec := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: %d, Cache-Control %q", path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// PLAN-004 phase 5 (B4): the dashboard API views, each checked by what it
// returns.

func viewServer(t *testing.T, kc *fake.Clientset, opts Options, rec *events.Recorder) *Server {
	t.Helper()
	if rec == nil {
		rec = events.NewRecorder(50)
	}
	opts.DashboardToken = "tok"
	if opts.AllowNamespace == nil {
		opts.AllowNamespace = func(ns string) bool { return ns == "default" }
	}
	return NewServer(":0", rec, &AgentMeta{Version: "test"}, kc, opts)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func deny(res string) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource(res), "", errors.New("denied"))
	}
}

func TestEventsView_LimitAndTypeFilter(t *testing.T) {
	rec := events.NewRecorder(50)
	for i := 0; i < 6; i++ {
		rec.Record(events.Event{Type: events.Incident, Reason: "crash"})
		rec.Record(events.Event{Type: events.Scaling, Reason: "scale"})
	}
	s := viewServer(t, fake.NewSimpleClientset(), Options{}, rec)
	if got := decode[[]events.Event](t, call(s, "GET", "/api/events?limit=3", "")); len(got) != 3 {
		t.Fatalf("limit=3 returned %d", len(got))
	}
	got := decode[[]events.Event](t, call(s, "GET", "/api/events?type=scaling&limit=4", ""))
	if len(got) != 4 {
		t.Fatalf("type filter with limit returned %d", len(got))
	}
	for _, e := range got {
		if e.Type != events.Scaling {
			t.Fatalf("type filter let through %s", e.Type)
		}
	}
	if got := decode[[]events.Event](t, call(s, "GET", "/api/events?limit=9999", "")); len(got) != 12 {
		t.Fatalf("a limit over the cap returns what there is, got %d", len(got))
	}
	if got := decode[[]events.Event](t, call(s, "GET", "/api/events?limit=nonsense", "")); len(got) != 12 {
		t.Fatalf("a bad limit falls back to the default, got %d", len(got))
	}
	if stats := decode[map[string]any](t, call(s, "GET", "/api/stats", "")); stats["total"].(float64) != 12 {
		t.Fatalf("stats: %v", stats)
	}
}

func TestNodesView_StatusRolesPressureAndErrors(t *testing.T) {
	nodes := []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-49 * time.Hour))},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
					{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}, {Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
			Spec: corev1.NodeSpec{Unschedulable: true},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w2"}},
	}
	s := viewServer(t, fake.NewSimpleClientset(nodes...), Options{}, nil)
	by := map[string]nodeInfo{}
	for _, n := range decode[[]nodeInfo](t, call(s, "GET", "/api/nodes", "")) {
		by[n.Name] = n
	}
	if cp := by["cp"]; cp.Status != "Ready" || cp.Roles != "control-plane" || !cp.MemoryPressure || !cp.DiskPressure || cp.Memory != "8.0Gi" || cp.Age != "2d" {
		t.Fatalf("control plane node: %+v", cp)
	}
	if w1 := by["w1"]; w1.Status != "NotReady" || w1.Roles != "worker" || !w1.Unschedulable || w1.Memory != "512Mi" {
		t.Fatalf("cordoned worker: %+v", w1)
	}
	if w2 := by["w2"]; w2.Status != "Unknown" || w2.Roles != "worker" {
		t.Fatalf("node with no conditions or role label: %+v", w2)
	}

	kc := fake.NewSimpleClientset()
	kc.PrependReactor("list", "nodes", deny("nodes"))
	if rec := call(viewServer(t, kc, Options{}, nil), "GET", "/api/nodes", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a node list that fails is an error, got %d", rec.Code)
	}
}

func TestClusterEventsView_Scoped(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	evs := []runtime.Object{
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Reason: "Scheduled", LastTimestamp: old,
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"}, Source: corev1.EventSource{Component: "scheduler"}, Count: 1},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default", CreationTimestamp: old}, Reason: "BackOff", Type: "Warning",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "kube-system"}, Reason: "Hidden"},
	}
	s := viewServer(t, fake.NewSimpleClientset(evs...), Options{}, nil)
	got := decode[[]k8sEvent](t, call(s, "GET", "/api/k8s-events", ""))
	if len(got) != 2 {
		t.Fatalf("only watched namespaces are shown, got %+v", got)
	}
	for _, e := range got {
		if e.Reason == "Hidden" || e.Age != "2h" {
			t.Fatalf("event %+v (a missing last time falls back to creation)", e)
		}
	}
	if one := decode[[]k8sEvent](t, call(s, "GET", "/api/k8s-events?namespace=default", "")); len(one) != 2 {
		t.Fatalf("one namespace: %d", len(one))
	}
	if rec := call(s, "GET", "/api/k8s-events?namespace=kube-system", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a namespace outside the scope is refused, got %d", rec.Code)
	}
	kc := fake.NewSimpleClientset()
	kc.PrependReactor("list", "events", deny("events"))
	if rec := call(viewServer(t, kc, Options{}, nil), "GET", "/api/k8s-events", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed event list is an error, got %d", rec.Code)
	}
}

func TestCostView_ComputedFromRequests(t *testing.T) {
	two := int32(2)
	reqs := corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")}}
	spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: reqs}}}
	node := func(name, label, typ, cpu, mem string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{label: typ}},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}}}
	}
	objs := []runtime.Object{
		node("n1", "node.kubernetes.io/instance-type", "m5.large", "2", "8Gi"),
		node("n2", "beta.kubernetes.io/instance-type", "custom.big", "4", "16Gi"),
		node("n3", "unrelated", "x", "1", "1Gi"),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"}, Spec: spec},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &two, Template: corev1.PodTemplateSpec{Spec: spec}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "tiny", Namespace: "default"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: spec}}},
	}
	cfg := config.Cost{CPUPerHour: "0.04", MemPerGiBHour: "0.005", InstancePrices: "custom.big=0.5"}
	s := viewServer(t, fake.NewSimpleClientset(objs...), Options{Cost: cfg}, nil)
	got := decode[clusterCost](t, call(s, "GET", "/api/cost", ""))
	src := map[string]string{}
	for _, n := range got.Nodes {
		src[n.Name] = n.PriceSource
	}
	if src["n1"] != "aws-known" || src["n2"] != "instance-type" || src["n3"] != "computed" {
		t.Fatalf("price sources: %v", src)
	}
	if len(got.Namespaces) != 1 || got.Namespaces[0].Name != "default" || got.Namespaces[0].Pods != 1 || got.Namespaces[0].CPUReq != 0.5 {
		t.Fatalf("only watched namespaces are costed: %+v", got.Namespaces)
	}
	if len(got.TopWorkloads) != 3 || got.TopWorkloads[0].Name != "api" || got.TopWorkloads[0].Replicas != 2 || got.TopWorkloads[2].Monthly != 0 {
		t.Fatalf("workloads, most expensive first: %+v", got.TopWorkloads)
	}
	if got.Summary.TotalNodes != 3 || got.Summary.TotalCPUCores != 7 || got.TotalMonthly <= 0 || got.WastedCost < 0 {
		t.Fatalf("summary: %+v total %v wasted %v", got.Summary, got.TotalMonthly, got.WastedCost)
	}

	kc := fake.NewSimpleClientset()
	kc.PrependReactor("list", "nodes", deny("nodes"))
	if rec := call(viewServer(t, kc, Options{Cost: cfg}, nil), "GET", "/api/cost", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed node list is an error, got %d", rec.Code)
	}
}

func TestCostView_AllocationSourcesAndFallback(t *testing.T) {
	alloc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`{"data":[{"default":{"totalCost":12.5},"shop":{"totalCost":7.5}}]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer alloc.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()

	kc := viewServer(t, fake.NewSimpleClientset(), Options{Cost: config.Cost{KubecostURL: alloc.URL}, HTTPClient: alloc.Client()}, nil)
	got := decode[clusterCost](t, call(kc, "GET", "/api/cost", ""))
	if got.Config.Source != "kubecost" || got.TotalMonthly != 20 || len(got.Namespaces) != 2 {
		t.Fatalf("kubecost answer: %+v", got)
	}

	failed := obs.HandlerErrorsTotal.WithLabelValues("cost", "kubecost")
	before := testutil.ToFloat64(failed)
	both := viewServer(t, fake.NewSimpleClientset(), Options{Cost: config.Cost{KubecostURL: down.URL, OpenCostURL: alloc.URL}, HTTPClient: alloc.Client()}, nil)
	if got := decode[clusterCost](t, call(both, "GET", "/api/cost", "")); got.Config.Source != "opencost" {
		t.Fatalf("kubecost down falls back to opencost: %+v", got.Config)
	}
	if testutil.ToFloat64(failed) != before+1 {
		t.Fatal("a cost source that did not answer must be counted (ISS-098)")
	}
	computed := viewServer(t, fake.NewSimpleClientset(), Options{Cost: config.Cost{OpenCostURL: down.URL}, HTTPClient: down.Client()}, nil)
	if got := decode[clusterCost](t, call(computed, "GET", "/api/cost", "")); got.Config.Source != "computed" {
		t.Fatalf("with every source down the prices are computed and labelled so, got %q", got.Config.Source)
	}

	for body, wantErr := range map[string]bool{`not json`: true, `{"data":[]}`: false} {
		b := body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := w.Write([]byte(b)); err != nil {
				t.Error(err)
			}
		}))
		res, err := CostConfig{}.fetchAllocation(t.Context(), srv.Client(), "x", srv.URL)
		srv.Close()
		if wantErr != (err != nil) || (!wantErr && res.TotalMonthly != 0) {
			t.Fatalf("%q: %v %v", b, res, err)
		}
	}
}

func TestExtendedViews_ServeTheTrackers(t *testing.T) {
	lm := kube.NewLearningMode(t.TempDir()+"/b.json", time.Hour)
	lm.RecordCPU("default", "deployment/api", 0.4)
	dt := kube.NewDeployTracker(10)
	dt.RecordDeployAt("default", "api", "api:v2", 2, 2, time.Now())
	dr := kube.NewDryRunLog(10)
	dr.Record(kube.DryRunEntry{Namespace: "default", Workload: "deployment/api", Action: "delete_pod"})
	ft := kube.NewFixTracker(10)
	ft.RecordAction("default", "deployment/api", "api-1", "CrashLoopBackOff", "delete_pod")
	rec := events.NewRecorder(10)
	rec.Record(events.Event{Type: events.Incident, Namespace: "default", Workload: "deployment/api", Action: "verified-fix", Reason: "CrashLoopBackOff"})
	s := viewServer(t, fake.NewSimpleClientset(), Options{Extended: ExtendedDeps{Learning: lm, Deploys: dt, DryRun: dr, Fixes: ft}}, rec)

	if b := decode[map[string]any](t, call(s, "GET", "/api/baselines", "")); b["learning"] != true || len(b["baselines"].(map[string]any)) != 1 {
		t.Fatalf("baselines: %v", b)
	}
	if d := decode[[]map[string]any](t, call(s, "GET", "/api/deploys", "")); len(d) != 1 {
		t.Fatalf("deploys: %v", d)
	}
	if d := decode[[]map[string]any](t, call(s, "GET", "/api/dry-run", "")); len(d) != 1 {
		t.Fatalf("dry run: %v", d)
	}
	if f := decode[map[string]any](t, call(s, "GET", "/api/fixes", "")); len(f["fixed"].([]any)) != 1 || len(f["pending"].([]any)) != 1 {
		t.Fatalf("fixes: %v", f)
	}
	for _, q := range []string{"?days=7", "?days=x"} {
		if c := call(s, "GET", "/api/compliance"+q, ""); c.Code != http.StatusOK {
			t.Fatalf("compliance%s: %d", q, c.Code)
		}
	}

	empty := viewServer(t, fake.NewSimpleClientset(), Options{}, nil)
	if b := decode[map[string]any](t, call(empty, "GET", "/api/baselines", "")); b["learning"] != false {
		t.Fatalf("baselines without learning: %v", b)
	}
	for _, p := range []string{"/api/deploys", "/api/dry-run"} {
		if d := decode[[]any](t, call(empty, "GET", p, "")); len(d) != 0 {
			t.Fatalf("%s without a tracker: %v", p, d)
		}
	}
}

func TestSlackActions_RefusesWhatItCannotTrust(t *testing.T) {
	h := NewSlackActionHandler("secret")
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", slackActionsPath, nil))
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", get.Code)
	}
	if rec := postSlack(NewSlackActionHandler(""), "payload={}", ts, "v0=x"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no signing secret: %d", rec.Code)
	}
	if rec := postSlack(h, "payload={}", ts, "v0=bad"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", rec.Code)
	}
	for body, want := range map[string]int{
		"%zz":                                http.StatusBadRequest, // not form encoded
		"nothing=here":                       http.StatusBadRequest, // no payload
		"payload=not-json":                   http.StatusBadRequest,
		`payload={"type":"view_submission"}`: http.StatusOK, // not a button press: ignored
	} {
		if rec := postSlack(h, body, ts, sign("secret", ts, body)); rec.Code != want {
			t.Fatalf("%q: %d, want %d", body, rec.Code, want)
		}
	}
	for action, want := range map[string]string{
		"approve_fix": "Approve is not available",
		"reject_fix":  "Reject is not available",
		"rollback":    "Unknown action: rollback",
	} {
		body := slackAction(action, "x", "U1")
		if rec := postSlack(h, body, ts, sign("secret", ts, body)); !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s without approvals: %q, want %q", action, rec.Body.String(), want)
		}
	}
}

func TestViewHelpers(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	crash := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{State: running}}}},
		{Status: corev1.PodStatus{Phase: corev1.PodPending}},
		{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
		{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
		{Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{State: crash}}}},
	}
	if s := summarizePods(pods); s != (podSummary{Total: 5, Running: 2, Pending: 1, Failed: 1, Succeeded: 1, CrashLoop: 1, NotReady: 1}) {
		t.Fatalf("summarizePods: %+v", s)
	}
	oom := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}
	initPull := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	initErr := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error"}}
	for want, p := range map[string]corev1.Pod{
		"CrashLoopBackOff":      pods[4],
		"OOMKilled":             {Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: oom}}}},
		"Init:ImagePullBackOff": {Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{State: initPull}}}},
		"Init:Error":            {Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{State: initErr}}}},
		"Pending":               pods[1],
	} {
		if got := podStatus(&p); got != want {
			t.Fatalf("podStatus = %q, want %q", got, want)
		}
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 5 * time.Minute: "5m", 3 * time.Hour: "3h", 50 * time.Hour: "2d"} {
		if got := age(time.Now().Add(-d)); got != want {
			t.Fatalf("age(%v) = %q, want %q", d, got, want)
		}
	}
	w := []workloadCost{{Name: "a", Monthly: 1}, {Name: "b", Monthly: 3}, {Name: "c", Monthly: 2}}
	sortWorkloads(w)
	if w[0].Name != "b" || w[2].Name != "a" {
		t.Fatalf("sortWorkloads: %+v", w)
	}
	if safePct(1, 0) != 0 || safePct(1, 4) != 25 {
		t.Fatal("safePct")
	}
}

func TestClusterAndNamespaceViews(t *testing.T) {
	two := int32(2)
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{
			{Name: "run", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			{Name: "wait", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			{Name: "done", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}}}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &two}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP}, {Port: 443, Protocol: corev1.ProtocolTCP}}}},
	}
	s := viewServer(t, fake.NewSimpleClientset(objs...), Options{}, nil)
	if body := call(s, "GET", "/api/cluster", "").Body.String(); !strings.Contains(body, `"default"`) || strings.Contains(body, "kube-system") {
		t.Fatalf("cluster view shows watched namespaces only: %s", body)
	}
	body := call(s, "GET", "/api/namespace/default", "").Body.String()
	for _, want := range []string{"30080", "443", "CrashLoopBackOff", `"bare"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("namespace view should mention %s: %s", want, body)
		}
	}
	if rec := call(s, "GET", "/api/namespace/", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no namespace: %d", rec.Code)
	}
	if rec := call(s, "GET", "/api/namespace/kube-system", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a namespace outside the scope: %d", rec.Code)
	}

	kc := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	for _, res := range []string{"pods", "deployments", "services", "jobs"} {
		kc.PrependReactor("list", res, deny(res))
	}
	failing := viewServer(t, kc, Options{}, nil)
	forbidden := obs.APIErrorsTotal.WithLabelValues("services", "forbidden")
	before := testutil.ToFloat64(forbidden)
	if rec := call(failing, "GET", "/api/cluster", ""); rec.Code != http.StatusOK {
		t.Fatalf("a namespace whose lists fail still shows: %d", rec.Code)
	}
	if rec := call(failing, "GET", "/api/namespace/default", ""); rec.Code != http.StatusOK {
		t.Fatalf("namespace view with failing lists: %d", rec.Code)
	}
	if testutil.ToFloat64(forbidden) < before+2 {
		t.Fatal("refused lists are counted in both views")
	}
	none := fake.NewSimpleClientset()
	none.PrependReactor("list", "namespaces", deny("namespaces"))
	for _, p := range []string{"/api/cluster", "/api/resources"} {
		if rec := call(viewServer(t, none, Options{}, nil), "GET", p, ""); rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s with no namespace list: %d", p, rec.Code)
		}
	}
}

func TestWriteJSON_CountsAFailedEncode(t *testing.T) {
	failed := obs.HandlerErrorsTotal.WithLabelValues("http", "encode")
	before := testutil.ToFloat64(failed)
	writeJSON(httptest.NewRecorder(), map[string]any{"bad": make(chan int)})
	if testutil.ToFloat64(failed) != before+1 {
		t.Fatal("a response that cannot be encoded must be counted (ISS-098)")
	}
}

func TestResourcesView_ClassifiesEveryPod(t *testing.T) {
	pod := func(name, cpuReq, cpuLim, memReq, memLim string) *corev1.Pod {
		rl := func(cpu, mem string) corev1.ResourceList {
			l := corev1.ResourceList{}
			if cpu != "" {
				l[corev1.ResourceCPU] = resource.MustParse(cpu)
			}
			if mem != "" {
				l[corev1.ResourceMemory] = resource.MustParse(mem)
			}
			return l
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app",
			Resources: corev1.ResourceRequirements{Requests: rl(cpuReq, memReq), Limits: rl(cpuLim, memLim)}}}}}
	}
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		pod("bare", "", "", "", ""),                    // no limits
		pod("greedy", "", "1", "", "1Gi"),              // limits, no requests: overuse
		pod("tiny", "5m", "500m", "64Mi", "128Mi"),     // tiny CPU request: underuse
		pod("right", "250m", "500m", "256Mi", "512Mi"), // right-sized
	}
	s := viewServer(t, fake.NewSimpleClientset(objs...), Options{}, nil)
	got := decode[[]nsResourceOverview](t, call(s, "GET", "/api/resources", ""))
	if len(got) != 1 || got[0].Name != "default" {
		t.Fatalf("watched namespaces only: %+v", got)
	}
	if o := got[0]; o.Pods != 4 || o.NoLimits != 1 || o.Overuse != 1 || o.Underuse != 1 || o.Healthy != 1 {
		t.Fatalf("every class counted once: %+v", o)
	}
	if rec := call(s, "GET", "/api/resources/", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("no namespace: %d", rec.Code)
	}

	kc := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	kc.PrependReactor("list", "pods", deny("pods"))
	if got := decode[[]nsResourceOverview](t, call(viewServer(t, kc, Options{}, nil), "GET", "/api/resources", "")); len(got) != 1 || got[0].Pods != 0 {
		t.Fatalf("a namespace whose pods cannot be listed shows empty: %+v", got)
	}
}

func TestViews_WithoutAClusterAnswerEmpty(t *testing.T) {
	s := NewServer(":0", events.NewRecorder(1), &AgentMeta{Version: "test"}, nil, Options{DashboardToken: "tok", AllowNamespace: func(string) bool { return true }})
	for _, p := range []string{"/api/cost", "/api/resources", "/api/nodes", "/api/k8s-events", "/api/cluster", "/api/namespace/default", "/api/resources/default"} {
		if rec := call(s, "GET", p, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s without a cluster client: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

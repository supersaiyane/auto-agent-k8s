package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase 12 audit of VerifyFixes (leader loop).
//
// Claims: an action is "fixed" once its workload is healthy again, and
// "not fixed" after 15 minutes.
// Bug found: it copied the pending list, worked on the copy and wrote the
// result back, so an action a handler recorded during the pass was lost and
// never shown on the Fixes tab. Now actions added during the pass are kept.
func TestAudit_VerifyFixes(t *testing.T) {
	one := int32(1)
	healthy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Replicas: &one}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	h := newFindingHarness(t, healthy)
	now := testNow
	ft := NewFixTracker(10)
	ft.now = func() time.Time { return now }
	h.deps.FixTracker = ft
	ft.RecordAction("default", "replicaset/api-7d9f84fd6c", "api-7d9f84fd6c-x", "CrashLoopBackOff", "delete_pod")
	ft.RecordAction("default", "replicaset/gone-1", "gone-1-x", "CrashLoopBackOff", "delete_pod")
	now = now.Add(time.Minute)

	// A handler records an action while the pass reads the cluster.
	var once sync.Once
	kc := h.deps.Client
	h.deps.Client = &recordingDuring{Interface: kc, during: func() {
		once.Do(func() { ft.RecordAction("default", "replicaset/late-1", "late-1-x", "OOMKilled", "delete_pod") })
	}}
	VerifyFixes(context.Background(), h.deps)
	if f := ft.Fixed(); len(f) != 1 || f[0].Workload != "replicaset/api-7d9f84fd6c" {
		t.Fatalf("the healthy workload is verified: %+v", f)
	}
	var pending []string
	for _, p := range ft.Pending() {
		pending = append(pending, p.Workload)
	}
	if strings.Join(pending, ",") != "replicaset/gone-1,replicaset/late-1" {
		t.Fatalf("the unhealthy one stays pending and the late one is kept: %v", pending)
	}
	now = now.Add(20 * time.Minute)
	VerifyFixes(context.Background(), h.deps)
	if len(ft.Failed()) != 2 {
		t.Fatalf("after 15 minutes they are not fixed: %+v", ft.Failed())
	}
	VerifyFixes(context.Background(), &Deps{}) // no tracker: nothing to do
}

// Phase 12 audit of ScanDeployments (leader loop).
//
// Claims: records each new Deployment revision for the Deploys tab and for
// "recent deploy" correlation.
// Bug found: on start every existing Deployment was recorded as deployed
// "now", so the Deploys tab showed them all as fresh and incidents in the
// next ten minutes were blamed on deploys made weeks ago. Now each revision
// is dated by its rollout (the Progressing condition's last update).
func TestAudit_ScanDeployments(t *testing.T) {
	old := testNow.Add(-30 * 24 * time.Hour)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "7"}, CreationTimestamp: metav1.NewTime(old.Add(-time.Hour))},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "api:v7"}}}}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, LastUpdateTime: metav1.NewTime(old)}}}}
	bare := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "default", CreationTimestamp: metav1.NewTime(old)}}
	h := newFindingHarness(t, d, bare)
	h.deps.DeployTracker = NewDeployTracker(10)
	ScanDeployments(context.Background(), h.deps)
	ScanDeployments(context.Background(), h.deps)
	all := h.deps.DeployTracker.All()
	if len(all) != 2 {
		t.Fatalf("each revision once: %+v", all)
	}
	for _, r := range all {
		if !r.Timestamp.Equal(old.UTC()) {
			t.Errorf("%s dated %s, want its rollout %s", r.Deployment, r.Timestamp, old.UTC())
		}
	}
	if got := h.deps.DeployTracker.RecentDeploys("default", 10*time.Minute); len(got) != 0 {
		t.Fatalf("a month-old deploy is not recent: %+v", got)
	}
	h.deps.DeployTracker.RecordDeploy("default", "fresh", "x:1", 1, 1)
	h.deps.DeployTracker.RecordDeployAt("default", "older", "x:1", 1, 1, time.Now().Add(-time.Hour))
	if got := h.deps.DeployTracker.RecentDeploys("default", 10*time.Minute); len(got) != 1 || got[0].Deployment != "fresh" {
		t.Fatalf("an old record after a new one does not hide it: %+v", got)
	}
	expectCounted(t, "deployments", func(d *Deps) { d.DeployTracker = NewDeployTracker(1); ScanDeployments(context.Background(), d) })
}

// Phase 12 audit of CollectBaselines (leader loop).
//
// Claims: samples each Deployment's CPU into learned baselines.
// Bug found: a failed Deployment list was dropped; now counted.
func TestAudit_CollectBaselines(t *testing.T) {
	h := newFindingHarness(t, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}})
	h.deps.Metrics = &mockMetrics{cpu: 0.4}
	h.deps.LearningMode = NewLearningMode("", time.Hour)
	CollectBaselines(context.Background(), h.deps)
	if b := h.deps.LearningMode.GetBaseline("default", "api"); b == nil || b.SampleCount != 1 {
		t.Fatalf("one sample recorded: %+v", b)
	}
	CollectBaselines(context.Background(), &Deps{}) // learning off
	expectCounted(t, "deployments", func(d *Deps) { d.LearningMode = NewLearningMode("", time.Hour); CollectBaselines(context.Background(), d) })
}

// Phase 12 audit of SelfCheck (every controller).
//
// Claims: reports when Prometheus, Slack or Alertmanager cannot be reached.
// Finding: a branch special-cased Slack's 400, 404 and 405 replies, which
// could never reach it, because httpCheck fails only on 5xx; removed.
func TestAudit_SelfCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }))
	defer ok.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer broken.Close()

	h := newFindingHarness(t)
	h.deps.HTTPClient = ok.Client()
	h.deps.Endpoints = SelfCheckEndpoints{PrometheusURL: ok.URL, SlackWebhookURL: ok.URL, AlertmanagerURL: ok.URL}
	SelfCheck(context.Background(), h.deps)
	h.quiet(t)

	h2 := newFindingHarness(t)
	h2.deps.HTTPClient = broken.Client()
	h2.deps.Endpoints = SelfCheckEndpoints{PrometheusURL: broken.URL, SlackWebhookURL: broken.URL, AlertmanagerURL: "http://127.0.0.1:1"}
	SelfCheck(context.Background(), h2.deps)
	msgs := strings.Join(h2.slack.Messages(), "\n")
	for _, want := range []string{"Prometheus unreachable", "Slack webhook unreachable: status 502", "Alertmanager unreachable"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("self check lacks %q:\n%s", want, msgs)
		}
	}
	if evs := h2.rec.Recent(10); len(evs) != 1 || evs[0].Reason != "SelfCheckFailed" {
		t.Fatalf("one event: %+v", evs)
	}
}

package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Phase 12 audit (ISS-039) of CheckStuckRollouts.
//
// Claims: a Deployment whose rollout passed its progress deadline is rolled
// back to the previous ReplicaSet (R4).
// Condition: Progressing=False with reason ProgressDeadlineExceeded.
// False negatives: a paused Deployment never reports the deadline (covered by
// CheckDeploymentPaused); a rollout without progressDeadlineSeconds never
// gets the condition.
// False positives: none known; the condition is set by the controller only.
// Bug found: a failed Deployment list was a verbose log and a silent continue;
// now counted. The finding now carries rung R4 in the event and message.
func TestAudit_StuckRollout(t *testing.T) {
	two := int32(2)
	stuck := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}}}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet api-2 has timed out"}}}}
	healthy := stuck.DeepCopy()
	healthy.Name = "web"
	healthy.Status.Conditions[0] = appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}
	rs := func(name, rev, image string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}},
			Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}}}}}
	}
	h := newFindingHarness(t, stuck, healthy, rs("api-1", "1", "api:v1"), rs("api-2", "2", "api:v2"))
	openGuardrails(h.deps)
	CheckStuckRollouts(context.Background(), h.deps)

	evs := h.rec.Recent(100)
	var got []string
	for _, e := range evs {
		if e.Type == "incident" {
			got = append(got, e.Workload+":"+e.Rung)
		}
	}
	if strings.Join(got, ",") != "api:R4" {
		t.Fatalf("only the stuck rollout is reported, at R4: %v", got)
	}
	d, err := h.deps.Client.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil || d.Spec.Template.Spec.Containers[0].Image != "api:v1" {
		t.Fatalf("rolled back to revision 1: %v %+v", err, d.Spec.Template.Spec.Containers)
	}
	msgs := h.slack.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "_Rung R4, automatic fix_") || !strings.Contains(msgs[0], "rolled back from revision 2 to 1") {
		t.Fatalf("message: %v", msgs)
	}
	expectCounted(t, "deployments", func(d *Deps) { CheckStuckRollouts(context.Background(), d) })
}

// Phase 12 audit of CheckServiceEndpoints.
//
// Claims: a Service that sends traffic nowhere is reported.
// Condition (before): an Endpoints object with subsets and no ready address.
// False negatives (before, ISS-034): a selector that matches no pod gives an
// Endpoints object with no subsets, so the worst case was never reported.
// It also read the deprecated Endpoints API (ISS-035).
// Condition (now): a Service with a selector, not ExternalName, older than
// serviceGrace, with no ready address in its EndpointSlices. The finding
// says whether no pod matches (naming the closest pods) or pods match and
// none is ready. Rung R1: labels are intent.
// Known misses, on purpose: Services without a selector (their endpoints are
// managed by hand) and Services younger than serviceGrace.
func TestAudit_ServiceEndpoints(t *testing.T) {
	old := metav1.NewTime(testNow.Add(-time.Hour))
	svc := func(name string, sel map[string]string, mut func(*corev1.Service)) *corev1.Service {
		s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: old},
			Spec: corev1.ServiceSpec{Selector: sel}}
		if mut != nil {
			mut(s)
		}
		return s
	}
	yes, no := true, false
	slice := func(svcName string, ready *bool) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: svcName + "-x", Namespace: "default",
			Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ready}}}}
	}
	typo := pod("api-1", func(p *corev1.Pod) { p.Labels = map[string]string{"app": "api", "tier": "bakend"} })
	notReady := pod("web-1", func(p *corev1.Pod) { p.Labels = map[string]string{"app": "web"} })
	objs := []runtime.Object{
		svc("api", map[string]string{"app": "api", "tier": "backend"}, nil), // ISS-034: matches no pod
		svc("web", map[string]string{"app": "web"}, nil),                    // a pod matches, not ready
		svc("ok", map[string]string{"app": "ok"}, nil), slice("ok", &yes),
		svc("nilready", map[string]string{"app": "n"}, nil), slice("nilready", nil), // nil means ready
		slice("web", &no),
		svc("young", map[string]string{"app": "y"}, func(s *corev1.Service) { s.CreationTimestamp = metav1.NewTime(testNow) }),
		svc("manual", nil, nil),
		svc("ext", map[string]string{"app": "e"}, func(s *corev1.Service) { s.Spec.Type = corev1.ServiceTypeExternalName }),
		typo, notReady,
	}
	h := newFindingHarness(t, objs...)
	CheckServiceEndpoints(context.Background(), h.deps)
	msgs := h.expect(t, "NoEndpoints", RungGuided, 2)
	joined := strings.Join(msgs, "\n")
	for _, want := range []string{
		"`default/service/api`: the selector matches no pod",
		"Selector: `app=api,tier=backend`",
		"Closest pod: `api-1` matches 1 of 2 selector labels; it has tier=bakend",
		"`default/service/web`: 1 pod(s) match the selector but none is ready",
		"`web-1` (Running, not ready)",
		"kubectl describe pods -n default -l app=web",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("messages lack %q:\n%s", want, joined)
		}
	}
	if got := closestPods(map[string]string{"app": "x"}, nil); len(got) != 1 || !strings.Contains(got[0], "No pod") {
		t.Fatalf("no near pod: %v", got)
	}
	expectCounted(t, "services", func(d *Deps) { CheckServiceEndpoints(context.Background(), d) })
	h2 := newFindingHarness(t, svc("api", map[string]string{"app": "api"}, nil))
	forbid(t, h2.deps, "endpointslices")
	before := forbidden("endpointslices")
	CheckServiceEndpoints(context.Background(), h2.deps)
	if forbidden("endpointslices") <= before {
		t.Fatal("a forbidden EndpointSlice list is counted")
	}
	h2.quiet(t)
}

// Phase 12 audit of CheckFailedJobs.
//
// Claims: a failed Job is reported with the last attempt's logs and the
// guidance for its failure reason (R1); a CronJob's failed Jobs older than
// an hour are cleaned up through the gate (R4).
// Bugs found: (1) a failed Job left in the cluster was reported again every
// dedup window, forever; now once per Job UID, and not at all after
// failedJobLookback. (2) "The most recent pod" was the last list item, which
// the API does not order; now the newest by creation time. (3) List errors
// were dropped; now counted. (4) The cleanup cutoff read the real clock.
func TestAudit_FailedJobs(t *testing.T) {
	failed := func(name string, at time.Time, owner string) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name),
			CreationTimestamp: metav1.NewTime(at.Add(-time.Minute))},
			Status: batchv1.JobStatus{Failed: 3, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit", LastTransitionTime: metav1.NewTime(at)}}}}
		if owner != "" {
			j.OwnerReferences = []metav1.OwnerReference{{Kind: "CronJob", Name: owner}}
		}
		return j
	}
	running := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default"}}
	h := newFindingHarness(t,
		failed("nightly-1", testNow.Add(-10*time.Minute), "nightly"), // recent: reported
		failed("nightly-0", testNow.Add(-3*time.Hour), "nightly"),    // older than 1h: cleaned up, but still news
		failed("ancient", testNow.Add(-72*time.Hour), ""),            // past the lookback: quiet
		running)
	openGuardrails(h.deps)
	CheckFailedJobs(context.Background(), h.deps)
	CheckFailedJobs(context.Background(), h.deps) // a second pass reports nothing new
	var jobs []string
	for _, e := range h.rec.Recent(100) {
		if e.Reason == "JobFailed" && e.Type == "incident" {
			jobs = append(jobs, e.Workload)
		}
	}
	if strings.Join(jobs, ",") != "job/nightly-0,job/nightly-1" && strings.Join(jobs, ",") != "job/nightly-1,job/nightly-0" {
		t.Fatalf("each recent failure once, the ancient one never: %v", jobs)
	}
	joined := strings.Join(h.slack.Messages(), "\n")
	for _, want := range []string{"backoffLimit (6)", "(CronJob: `nightly`)", "cleaned up 1 old failed jobs"} {
		if !strings.Contains(joined, want) {
			t.Errorf("messages lack %q:\n%s", want, joined)
		}
	}
	if _, err := h.deps.Client.BatchV1().Jobs("default").Get(context.Background(), "nightly-0", metav1.GetOptions{}); err == nil {
		t.Error("the CronJob's failed job older than an hour is deleted")
	}

	a, b := pod("a", nil), pod("b", nil)
	a.CreationTimestamp, b.CreationTimestamp = metav1.NewTime(testNow), metav1.NewTime(testNow.Add(-time.Hour))
	if newestPod([]corev1.Pod{*a, *b}).Name != "a" || newestPod([]corev1.Pod{*b, *a}).Name != "a" || newestPod(nil) != nil {
		t.Fatal("the newest pod is chosen whatever the list order")
	}
	expectCounted(t, "jobs", func(d *Deps) { CheckFailedJobs(context.Background(), d) })
}

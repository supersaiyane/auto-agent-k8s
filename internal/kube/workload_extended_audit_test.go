package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// readyFor sets a pod's Ready condition, flipped d before testNow.
func readyFor(p *corev1.Pod, ready bool, d time.Duration) {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: st, LastTransitionTime: metav1.NewTime(testNow.Add(-d))}}
}

func labelled(name string, lbl map[string]string, mut func(*corev1.Pod)) *corev1.Pod {
	return pod(name, func(p *corev1.Pod) {
		p.Labels = lbl
		if mut != nil {
			mut(p)
		}
	})
}

// Phase 12 audit of CheckStatefulSetStuck.
//
// Claims: a StatefulSet stuck on one pod during ordered startup.
// Bug found: the "stuck for more than five minutes" check was a loop with a
// bare continue that did nothing, so every StatefulSet in a normal rolling
// update or scale-up was reported at once. Now the lowest-ordinal pod that
// is not ready must have stayed so for stuckFor, and the finding names it
// and its waiting reason (R1).
// Known miss: pods that could not be created at all (no pod to time); the
// ReplicaSet-style failure shows in StatefulSet events.
func TestAudit_StatefulSetStuck(t *testing.T) {
	three := int32(3)
	sts := func(name string) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:   appsv1.StatefulSetSpec{Replicas: &three, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}}
	}
	lbl := func(app string) map[string]string { return map[string]string{"app": app} }
	objs := []runtime.Object{
		sts("db"),
		labelled("db-0", lbl("db"), func(p *corev1.Pod) { readyFor(p, true, time.Hour) }),
		labelled("db-1", lbl("db"), func(p *corev1.Pod) {
			readyFor(p, false, 20*time.Minute)
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "db", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
		}),
		labelled("db-2", lbl("db"), func(p *corev1.Pod) { readyFor(p, false, 30*time.Minute) }),
		sts("cache"), // rolling: its not-ready pod flipped a minute ago
		labelled("cache-0", lbl("cache"), func(p *corev1.Pod) { readyFor(p, true, time.Hour) }),
		labelled("cache-1", lbl("cache"), func(p *corev1.Pod) { readyFor(p, false, time.Minute) }),
	}
	h := newFindingHarness(t, objs...)
	CheckStatefulSetStuck(context.Background(), h.deps)
	msg := h.expect(t, "StatefulSetStuck", RungGuided, 1)[0]
	for _, want := range []string{"statefulset/db", "1/3 ready; pod `db-1` has not been ready for 20m0s", "db: CrashLoopBackOff", "Ordered startup"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if ordinal("web") != 1<<30 || ordinal("db-12") != 12 {
		t.Fatal("ordinal parsing")
	}
	expectCounted(t, "statefulsets", func(d *Deps) { CheckStatefulSetStuck(context.Background(), d) })
}

// Phase 12 audit of CheckDaemonSetMissing.
//
// Claims: a DaemonSet that does not run a ready pod on every node it should.
// Bug found: any moment with ready below desired, such as every rolling
// update or a node joining, was reported at once. Now a pod must stay not
// ready for stuckFor, or, once the rollout is complete, nodes must have no
// pod at all. Rung R1, R3 (delete the stuck pod) with the approval queue.
func TestAudit_DaemonSetMissing(t *testing.T) {
	ds := func(name string, st appsv1.DaemonSetStatus) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 2},
			Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}}, Status: st}
	}
	lbl := func(app string) map[string]string { return map[string]string{"app": app} }
	objs := []runtime.Object{
		ds("logs", appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 3, NumberReady: 2, UpdatedNumberScheduled: 3, ObservedGeneration: 2}),
		labelled("logs-a", lbl("logs"), func(p *corev1.Pod) { p.Spec.NodeName = "node-a"; readyFor(p, false, 10*time.Minute) }),
		ds("rolling", appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 2, NumberReady: 2, UpdatedNumberScheduled: 1, ObservedGeneration: 2}),
		labelled("rolling-a", lbl("rolling"), func(p *corev1.Pod) { readyFor(p, false, time.Minute) }),
		ds("tainted", appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, CurrentNumberScheduled: 2, NumberReady: 2, UpdatedNumberScheduled: 3, ObservedGeneration: 2}),
		ds("fine", appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 2}),
	}
	h := newFindingHarness(t, objs...)
	CheckDaemonSetMissing(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "DaemonSetMissing", RungGuided, 2), "\n")
	for _, want := range []string{"daemonset/logs", "`logs-a` on `node-a`", "daemonset/tainted", "1 node(s) have no pod at all", "approval queue"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if strings.Contains(msgs, "rolling") {
		t.Error("a DaemonSet still rolling out is not reported")
	}
	expectCounted(t, "daemonsets", func(d *Deps) { CheckDaemonSetMissing(context.Background(), d) })
}

// Phase 12 audit of CheckCronJobMissed.
//
// Claims: a CronJob that missed a scheduled start.
// Bugs found: it looked only when the concurrency policy was Forbid and a
// job was active, and matched the words "missed" or "MissSchedule" anywhere
// in an event line. Now any CronJob whose controller emitted MissSchedule or
// TooManyMissedTimes in the last hour is reported; suspended ones are not.
// Rung R1, R3 (run it now) with the approval queue.
func TestAudit_CronJobMissed(t *testing.T) {
	cj := func(name string, suspend bool) *batchv1.CronJob {
		return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: batchv1.CronJobSpec{Schedule: "*/5 * * * *", Suspend: &suspend, ConcurrencyPolicy: batchv1.AllowConcurrent}}
	}
	ev := func(name, obj, reason string, at time.Time) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Reason: reason,
			Message:        "Missed scheduled time to start a job: 2026-10-08T11:55:00Z",
			InvolvedObject: corev1.ObjectReference{Kind: "CronJob", Name: obj}, LastTimestamp: metav1.NewTime(at)}
	}
	h := newFindingHarness(t, cj("report", false), cj("paused", true), cj("old", false), cj("other", false),
		ev("e1", "report", "MissSchedule", testNow.Add(-5*time.Minute)),
		ev("e2", "paused", "MissSchedule", testNow.Add(-5*time.Minute)),
		ev("e3", "old", "MissSchedule", testNow.Add(-3*time.Hour)),
		ev("e4", "other", "SawCompletedJob", testNow))
	CheckCronJobMissed(context.Background(), h.deps)
	msgs := h.expect(t, "CronJobMissed", RungGuided, 1)
	for _, want := range []string{"cronjob/report", "Missed scheduled time", "kubectl create job --from=cronjob/report"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("message lacks %q:\n%s", want, msgs[0])
		}
	}
	at := metav1.NewTime(testNow)
	if !eventTime(&corev1.Event{EventTime: metav1.NewMicroTime(testNow)}).Equal(testNow) ||
		!eventTime(&corev1.Event{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: at}}).Equal(testNow) {
		t.Fatal("eventTime falls back to EventTime, then creation")
	}
	expectCounted(t, "cronjobs", func(d *Deps) { CheckCronJobMissed(context.Background(), d) })
}

// Phase 12 audit of CheckDeploymentPaused.
//
// Claims: a Deployment someone paused and forgot.
// Bug found: any paused Deployment was reported the moment it was paused,
// then every dedup window. Now only after pausedFor, then once a day.
// Rung R1, R3 (resume) with the approval queue.
func TestAudit_DeploymentPaused(t *testing.T) {
	paused := func(name string, d time.Duration) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: appsv1.DeploymentSpec{Paused: true},
			Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
				Status: corev1.ConditionUnknown, Reason: "DeploymentPaused", LastTransitionTime: metav1.NewTime(testNow.Add(-d))}}}}
	}
	running := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "default"}}
	unknown := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "nostatus", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Paused: true}}
	h := newFindingHarness(t, paused("forgotten", 3*time.Hour), paused("justnow", time.Minute), running, unknown)
	CheckDeploymentPaused(context.Background(), h.deps)
	CheckDeploymentPaused(context.Background(), h.deps)
	msg := h.expect(t, "DeploymentPaused", RungGuided, 1)[0]
	if !strings.Contains(msg, "deployment/forgotten") || !strings.Contains(msg, "paused for 3h0m0s") || !strings.Contains(msg, "rollout resume deployment/forgotten") {
		t.Fatalf("message: %s", msg)
	}
	expectCounted(t, "deployments", func(d *Deps) { CheckDeploymentPaused(context.Background(), d) })
}

// Phase 12 audit of CheckReplicaSetFailure.
//
// Claims: a ReplicaSet the controller cannot create pods for.
// Bugs found: it required zero pods, so a ReplicaSet that created some and
// was refused the rest (quota half used) was missed; and it spent the dedup
// key before checking the condition. Now any ReplicaFailure=True is
// reported with the controller's reason (R1).
func TestAudit_ReplicaSetFailure(t *testing.T) {
	rs := func(name string, have int32, cond bool) *appsv1.ReplicaSet {
		four := int32(4)
		r := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: appsv1.ReplicaSetSpec{Replicas: &four}, Status: appsv1.ReplicaSetStatus{Replicas: have}}
		if cond {
			r.Status.Conditions = []appsv1.ReplicaSetCondition{{Type: appsv1.ReplicaSetReplicaFailure, Status: corev1.ConditionTrue,
				Reason: "FailedCreate", Message: `pods "api-x" is forbidden: exceeded quota: compute`}}
		}
		return r
	}
	h := newFindingHarness(t, rs("partial", 2, true), rs("empty", 0, true), rs("healthy", 4, false))
	CheckReplicaSetFailure(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "ReplicaSetFailure", RungGuided, 2), "\n")
	for _, want := range []string{"replicaset/partial`: cannot create pods: 2 of 4 exist", "exceeded quota: compute", "replicaset/empty"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	expectCounted(t, "replicasets", func(d *Deps) { CheckReplicaSetFailure(context.Background(), d) })
}

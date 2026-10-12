package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// PLAN-004 phase 5 (B4): every terminal command on its other paths: the
// flags each kind takes, a failing API, and the rollout and logs targets.

func run(t *testing.T, e *termEnv, cmd string) (string, error) {
	t.Helper()
	return executeKubectl(context.Background(), e, cmd)
}

func TestTerminal_EveryGetKindTakesItsFlags(t *testing.T) {
	e := termCluster(t)
	for _, k := range getKinds() {
		for _, flags := range []string{"-A -o wide", "-l app=api", "--field-selector metadata.name=api"} {
			cmd := "get " + k.name + " " + flags
			if k.cluster && strings.HasPrefix(flags, "-A") {
				cmd = "get " + k.name + " -o wide"
			}
			if _, err := run(t, e, cmd); err != nil {
				t.Errorf("%q: %v", cmd, err)
			}
		}
	}
}

func TestTerminal_AFailingAPIIsAnError(t *testing.T) {
	e := termCluster(t)
	kc := fake.NewSimpleClientset()
	kc.PrependReactor("*", "*", deny("anything"))
	e.kc = kc
	for _, k := range getKinds() {
		if k.name == "autoremediationpolicies" {
			continue // read through the dynamic client
		}
		if out, err := run(t, e, "get "+k.name); err == nil {
			t.Errorf("get %s with a failing API: want an error, got %q", k.name, out)
		}
	}
	for _, c := range []string{"describe pod api-1", "describe deployment api", "describe sts db", "describe pvc data", "describe hpa api-hpa",
		"describe ingress site", "logs api-1", "events", "rollout status deploy/api", "rollout history deploy/api"} {
		if _, err := run(t, e, c); err == nil {
			t.Errorf("%q with a failing API: want an error", c)
		}
	}
}

func TestTerminal_RolloutStatusReadsLikeKubectl(t *testing.T) {
	e := termCluster(t)
	for cmd, want := range map[string]string{
		"rollout status deploy/api":     `deployment "api" successfully rolled out`,
		"rollout status deployment/api": `successfully rolled out`,
		"rollout status deploy/stuck":   `exceeded its progress deadline: timed out`,
		"rollout status deploy/rolling": `1 of 2 new replicas have been updated`,
		"rollout status sts/db":         `db`,
		"rollout history deploy/api":    "api:v2",
		"rollout history sts/db":        "1",
	} {
		out, err := run(t, e, cmd)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%q = %q, %v; want it to contain %q", cmd, out, err, want)
		}
	}
	for _, bad := range []string{"rollout status", "rollout status pod/api-1", "rollout status svc/api", "rollout status deploy/",
		"rollout status deploy/missing", "rollout history pod/api-1"} {
		if _, err := run(t, e, bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestDeploymentRollout_EveryStage(t *testing.T) {
	e := termCluster(t)
	d, err := e.kc.AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Generation, d.Status.ObservedGeneration = 2, 1
	if got := deploymentRollout(d); !strings.Contains(got, "spec update to be observed") {
		t.Fatalf("generation not observed: %q", got)
	}
	d.Status.ObservedGeneration = 2
	d.Status.Replicas = 3
	if got := deploymentRollout(d); !strings.Contains(got, "1 old replicas are pending termination") {
		t.Fatalf("old replicas: %q", got)
	}
	d.Status.Replicas, d.Status.AvailableReplicas = 2, 1
	if got := deploymentRollout(d); !strings.Contains(got, "1 of 2 updated replicas are available") {
		t.Fatalf("available: %q", got)
	}
}

func TestTerminal_LogsAndEventsTargets(t *testing.T) {
	e := termCluster(t)
	for _, cmd := range []string{"logs pod/api-1", "logs deploy/api", "logs api-1 -c app --tail 5 --previous --since 5m"} {
		if _, err := run(t, e, cmd); err != nil {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	for _, bad := range []string{"logs svc/api", "logs deploy/", "logs deploy/stuck", "logs api-1 --since forever"} {
		if _, err := run(t, e, bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if out, err := run(t, e, "events --for pod/api-1"); err != nil || !strings.Contains(out, "BackOff") {
		t.Fatalf("events for one pod: %q %v", out, err)
	}
	if out, err := run(t, e, "events --for po/api-1"); err != nil || !strings.Contains(out, "BackOff") {
		t.Fatalf("a kind alias matches: %q %v", out, err)
	}
	if out, err := run(t, e, "events --for deploy/api"); err != nil || out != "No events found." {
		t.Fatalf("an object with no events: %q %v", out, err)
	}
	if _, err := run(t, e, "events --for api-1"); err == nil {
		t.Fatal("--for needs kind/name")
	}
	if out, err := run(t, e, "events -A"); err != nil || strings.Contains(out, "kube-system") {
		t.Fatalf("events -A stays in the watch scope: %q %v", out, err)
	}
}

func TestKindMatches(t *testing.T) {
	for want, got := range map[string]string{"po": "Pod", "pods": "Pod", "deploy": "Deployment", "deployments": "Deployment",
		"sts": "StatefulSet", "statefulsets": "StatefulSet", "job": "Job"} {
		if !kindMatches(want, got) {
			t.Errorf("kindMatches(%q, %q) = false", want, got)
		}
	}
	if kindMatches("pod", "Deployment") {
		t.Error("different kinds must not match")
	}
}

func TestStatefulSetRollout_EveryStage(t *testing.T) {
	three := int32(3)
	s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Generation: 2}, Spec: appsv1.StatefulSetSpec{Replicas: &three},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1}}
	for _, step := range []struct {
		set  func()
		want string
	}{
		{func() {}, "spec update to be observed"},
		{func() { s.Status.ObservedGeneration, s.Status.ReadyReplicas = 2, 1 }, "Waiting for 2 pods to be ready"},
		{func() {
			s.Status.ReadyReplicas, s.Status.UpdateRevision, s.Status.CurrentRevision, s.Status.UpdatedReplicas = 3, "db-2", "db-1", 1
		}, "1 out of 3 new pods"},
		{func() { s.Status.CurrentRevision = "db-2" }, "complete 3 pods at revision db-2"},
	} {
		step.set()
		if got := statefulSetRollout(s); !strings.Contains(got, step.want) {
			t.Fatalf("statefulSetRollout = %q, want %q", got, step.want)
		}
	}
}

func TestEventOrder_UsesTheBestTimeAvailable(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	evs := []corev1.Event{
		{ObjectMeta: metav1.ObjectMeta{Name: "newest", CreationTimestamp: metav1.NewTime(base.Add(30 * time.Minute))}},
		{ObjectMeta: metav1.ObjectMeta{Name: "middle"}, EventTime: metav1.NewMicroTime(base.Add(10 * time.Minute))},
		{ObjectMeta: metav1.ObjectMeta{Name: "oldest"}, LastTimestamp: metav1.NewTime(base)},
	}
	sortEvents(evs)
	if evs[0].Name != "oldest" || evs[1].Name != "middle" || evs[2].Name != "newest" {
		t.Fatalf("oldest first by last, event or creation time: %s %s %s", evs[0].Name, evs[1].Name, evs[2].Name)
	}
}

func TestPodOf_PrefersARunningPod(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	pod := func(name string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{"app": "web"}}, Status: corev1.PodStatus{Phase: phase}}
	}
	e := &termEnv{kc: fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Selector: sel}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "idle"}}}},
		pod("web-a", corev1.PodPending), pod("web-b", corev1.PodRunning))}
	if got, err := podOf(context.Background(), e, "default", "web"); err != nil || got != "web-b" {
		t.Fatalf("a running pod is preferred: %q %v", got, err)
	}
	if _, err := podOf(context.Background(), e, "default", "idle"); err == nil || !strings.Contains(err.Error(), "has no pod") {
		t.Fatalf("a deployment with no pod: %v", err)
	}
	e.kc = fake.NewSimpleClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Selector: sel}},
		pod("web-a", corev1.PodPending))
	if got, err := podOf(context.Background(), e, "default", "web"); err != nil || got != "web-a" {
		t.Fatalf("with no running pod the first is used: %q %v", got, err)
	}
}

func TestDescribe_EventsThatCannotBeReadAreSaid(t *testing.T) {
	e := termCluster(t)
	kc := e.kc.(*fake.Clientset)
	kc.PrependReactor("list", "events", deny("events"))
	out, err := run(t, e, "describe pod api-1")
	if err != nil || !strings.Contains(out, "could not read events") {
		t.Fatalf("describe still answers and says the events were not read: %q %v", out, err)
	}
	p, err := kc.CoreV1().Pods("default").Get(context.Background(), "api-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0}}
	if _, err := kc.CoreV1().Pods("default").UpdateStatus(context.Background(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, e, "describe pod api-1"); err != nil || !strings.Contains(out, "Terminated") {
		t.Fatalf("a terminated container is described: %q %v", out, err)
	}
}

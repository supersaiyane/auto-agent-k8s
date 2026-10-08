package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
)

type fakeAgent struct{}

func (fakeAgent) Scope() string  { return "watch: all non-system namespaces\nfix: default" }
func (fakeAgent) Status() string { return "role=all leader=true mode=dry-run version=test" }
func (fakeAgent) Gate(ns string) []GateRow {
	return []GateRow{{"mode", false, "dry-run: fixes are simulated"}, {"fix scope", true, ns + " is in the fix scope"}}
}

// termCluster is a fake cluster with one object of every kind the terminal
// reads, in "default", plus one pod in "kube-system" (outside the scope).
func termCluster(t *testing.T) *termEnv {
	t.Helper()
	two, one := int32(2), int32(1)
	yes := true
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: old, Labels: map[string]string{"app": "api"}}
	}
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}
	tmpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "api:v2"}}}}
	deploy := &appsv1.Deployment{ObjectMeta: meta("api"), Spec: appsv1.DeploymentSpec{Replicas: &two, Selector: sel, Template: tmpl},
		Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2}}
	stuck := &appsv1.Deployment{ObjectMeta: meta("stuck"), Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "none"}}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "timed out"}}}}
	rolling := &appsv1.Deployment{ObjectMeta: meta("rolling"), Spec: appsv1.DeploymentSpec{Replicas: &two}, Status: appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 1}}
	rs := func(name, rev, image string) *appsv1.ReplicaSet {
		m := meta(name)
		m.Annotations = map[string]string{"deployment.kubernetes.io/revision": rev}
		m.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "api"}}
		return &appsv1.ReplicaSet{ObjectMeta: m, Spec: appsv1.ReplicaSetSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: image}}}}}}
	}
	podReady := &corev1.Pod{ObjectMeta: meta("api-1"), Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "app", Image: "api:v2"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.5", ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: 3, Image: "api:v2",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: old}}}}}}
	hidden := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "kube-system"}}
	sts := &appsv1.StatefulSet{ObjectMeta: meta("db"), Spec: appsv1.StatefulSetSpec{Replicas: &one, Template: tmpl},
		Status: appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "db-1", UpdateRevision: "db-1"}}
	cr := &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{Name: "db-1", Namespace: "default", OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db"}}}, Revision: 1}
	ev := &corev1.Event{ObjectMeta: meta("ev1"), Type: "Warning", Reason: "BackOff", Message: "restarting failed container",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-1"}, LastTimestamp: old}
	min := intstrFromInt(1)
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", CreationTimestamp: old}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.2"}}}},
		deploy, stuck, rolling, rs("api-1a", "1", "api:v1"), rs("api-2b", "2", "api:v2"), podReady, hidden, sts, cr, ev,
		&appsv1.DaemonSet{ObjectMeta: meta("agent"), Spec: appsv1.DaemonSetSpec{Template: tmpl}},
		&batchv1.Job{ObjectMeta: meta("migrate"), Spec: batchv1.JobSpec{Template: tmpl}, Status: batchv1.JobStatus{Succeeded: 1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}},
		&batchv1.CronJob{ObjectMeta: meta("nightly"), Spec: batchv1.CronJobSpec{Schedule: "0 2 * * *"}},
		&corev1.Service{ObjectMeta: meta("api-svc"), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10", Selector: map[string]string{"app": "api"},
			Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}}}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api-svc-x", Namespace: "default", Labels: map[string]string{"kubernetes.io/service-name": "api-svc"}},
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &yes}}}},
		&networkingv1.Ingress{ObjectMeta: meta("site"), Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "shop.example.test",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/",
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api-svc"}}}}}}}}}},
		&networkingv1.NetworkPolicy{ObjectMeta: meta("deny"), Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: meta("api-hpa"), Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5,
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}}},
		&policyv1.PodDisruptionBudget{ObjectMeta: meta("api-pdb"), Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &min}},
		&corev1.PersistentVolumeClaim{ObjectMeta: meta("data"), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}},
		&corev1.ResourceQuota{ObjectMeta: meta("compute")},
		&corev1.ConfigMap{ObjectMeta: meta("settings"), Data: map[string]string{"level": "secret-looking-value"}},
	}
	kc := fake.NewSimpleClientset(objs...)
	kc.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = r.Spec.ResourceAttributes.Verb == "list"
		return true, r, nil
	})
	gvr := schema.GroupVersionResource{Group: "autoagent.io", Version: "v1alpha1", Resource: "autoremediationpolicies"}
	pol := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "autoagent.io/v1alpha1", "kind": "AutoRemediationPolicy",
		"metadata": map[string]any{"name": "api-policy", "namespace": "default"}}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{gvr: "AutoRemediationPolicyList"}, pol)
	rec := events.NewRecorder(10)
	rec.Record(events.Event{Type: events.Incident, Namespace: "default", Pod: "api-1", Reason: "CrashLoopBackOff", Message: "exit 1"})
	rec.Record(events.Event{Type: events.Audit, Namespace: "default", Pod: "api-1", Reason: "CrashLoopBackOff", Result: "simulated", Message: "delete pod"})
	rec.Record(events.Event{Type: events.Incident, Namespace: "default", Pod: "other", Reason: "OOMKilled"})
	return &termEnv{kc: kc, dyn: dyn, allowNS: func(ns string) bool { return ns == "default" }, agent: fakeAgent{}, recorder: rec, now: time.Now()}
}

// sample gives each table row a command that exercises it.
func sample(c termCmd) string {
	switch c.Verb {
	case "get":
		return "get " + c.Sub
	case "describe":
		return map[string]string{"pod": "describe pod api-1", "deployment": "describe deployment api", "statefulset": "describe sts db",
			"job": "describe job migrate", "service": "describe svc api-svc", "pvc": "describe pvc data", "hpa": "describe hpa api-hpa",
			"ingress": "describe ingress site", "node": "describe node node-1"}[c.Sub]
	case "logs":
		return "logs api-1"
	case "events":
		return "events"
	case "rollout":
		return "rollout " + c.Sub + " deploy/api"
	case "auth":
		return "auth can-i list pods"
	case "agent":
		if c.Sub == "why" {
			return "agent why api-1"
		}
		return "agent " + c.Sub
	}
	return c.Verb
}

// PLAN-003 1.1: every row of the command table runs.
func TestTerminal_EveryCommandRuns(t *testing.T) {
	e := termCluster(t)
	for _, c := range termCommands() {
		cmd := sample(c)
		if cmd == "" {
			t.Errorf("no sample for %s %s", c.Verb, c.Sub)
			continue
		}
		out, err := executeKubectl(context.Background(), e, cmd)
		if err != nil || strings.TrimSpace(out) == "" {
			t.Errorf("%q: %v %q", cmd, err, out)
		}
	}
}

// PLAN-003 1.1: every command on the never-added list is refused with its
// reason, and secrets are never read.
func TestTerminal_RefusesWithReason(t *testing.T) {
	e := termCluster(t)
	for _, cmd := range []string{"delete pod api-1", "apply -f x.yaml", "scale deploy/api --replicas=3", "edit deploy api", "patch deploy api",
		"cordon node-1", "drain node-1", "rollout restart deploy/api", "rollout undo deploy/api", "exec api-1 -- sh", "port-forward api-1 8080",
		"cp api-1:/etc/passwd x", "debug api-1", "get secrets", "describe secret db", "get secret db -n default"} {
		_, err := executeKubectl(context.Background(), e, cmd)
		if err == nil || !strings.Contains(err.Error(), "is not available:") {
			t.Errorf("%q: want a refusal with a reason, got %v", cmd, err)
		}
	}
	for _, r := range neverAdded() {
		if r.Why == "" {
			t.Errorf("%s has no reason", r.Commands)
		}
	}
	for _, bad := range []string{"", "get", "frobnicate", "get pods --bogus", "get pods -o yaml", "logs", "describe pod", "get pods --tail x", "get pods -n"} {
		if _, err := executeKubectl(context.Background(), e, bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	out, _ := executeKubectl(context.Background(), e, "get configmaps")
	if strings.Contains(out, "secret-looking-value") || !strings.Contains(out, "settings") {
		t.Fatalf("ConfigMaps show key counts, never values: %s", out)
	}
}

// PLAN-003 1.2: the help API serves the same table the terminal runs.
func TestTerminal_HelpAPI(t *testing.T) {
	s := NewServer(":0", events.NewRecorder(10), &AgentMeta{Version: "test"}, fake.NewSimpleClientset(), Options{DashboardToken: "dash"})
	r := httptest.NewRequest(http.MethodGet, "/api/kubectl/help", nil)
	r.Header.Set("Authorization", "Bearer dash")
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, r)
	var got struct {
		Commands []termCmd `json:"commands"`
		Refused  []refusal `json:"refused"`
		Rules    []string  `json:"rules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("help: %d %v", w.Code, err)
	}
	if len(got.Commands) != len(termCommands()) || len(got.Refused) != len(neverAdded()) || len(got.Rules) == 0 {
		t.Fatalf("help mirrors the table: %d commands, %d refusals", len(got.Commands), len(got.Refused))
	}
	if help := helpText(); !strings.Contains(help, "rollout status") || !strings.Contains(help, "Not available, and why") {
		t.Fatalf("help text: %s", help)
	}
}

// PLAN-003 2.1 to 2.6: what the new commands print.
func TestTerminal_Outputs(t *testing.T) {
	e := termCluster(t)
	for _, tc := range []struct{ cmd, want string }{
		{"get pods -o wide", "[ipv4]  node-1"}, // addresses are redacted like every output
		{"get pods -l app=api", "api-1"},
		{"get pods api-1", "Running"},
		{"get deploy -o wide", "api:v2"},
		{"get sts", "db"},
		{"get ds", "agent"},
		{"get rs -o wide", "deployment/api"},
		{"get jobs", "Complete"},
		{"get cronjobs", "0 2 * * *"},
		{"get svc -o wide", "app=api"},
		{"get endpointslices", "api-svc"},
		{"get ingress", "shop.example.test"},
		{"get netpol", "Ingress"},
		{"get hpa", "deployment/api"},
		{"get pdb", "api-pdb"},
		{"get pvc", "Bound"},
		{"get quota", "compute"},
		{"get arp", "api-policy"},
		{"get nodes -o wide", "INTERNAL-IP"},
		{"get events", "BackOff"},
		{"describe svc api-svc", "Ready endpoints:   1 [ipv4]"},
		{"describe ingress site", "shop.example.test/ -> api-svc"},
		{"describe deploy api", "2 desired"},
		{"describe pod api-1", "BackOff"},
		{"events --for pod/api-1", "restarting failed container"},
		{"logs deploy/api", "fake logs"},
		{"logs api-1 --previous --since=10m -c app --tail=5", "fake logs"},
		{"rollout status deploy/api", "successfully rolled out"},
		{"rollout status deploy/stuck", "exceeded its progress deadline"},
		{"rollout status deploy/rolling", "1 of 2 new replicas have been updated"},
		{"rollout status sts/db", "rolling update complete"},
		{"rollout history deploy/api", "api:v1"},
		{"rollout history sts/db", "db-1"},
		{"auth can-i list pods", "yes"},
		{"auth can-i delete pods", "no"},
		{"agent gate", "would not be applied now"},
		{"agent why api-1", "simulated"},
		{"agent scope", "fix: default"},
		{"agent status", "mode=dry-run"},
		{"get pods nothere", ""},
	} {
		out, err := executeKubectl(context.Background(), e, tc.cmd)
		if tc.want == "" {
			if err == nil {
				t.Errorf("%q: want not found", tc.cmd)
			}
			continue
		}
		if err != nil || !strings.Contains(out, tc.want) {
			t.Errorf("%q: want %q, got %v\n%s", tc.cmd, tc.want, err, out)
		}
	}
	if out, _ := executeKubectl(context.Background(), e, "agent why other-pod"); !strings.Contains(out, "recorded nothing") {
		t.Errorf("why with no records: %s", out)
	}
	if _, err := executeKubectl(context.Background(), e, "logs deploy/stuck"); err == nil || !strings.Contains(err.Error(), "has no pod") {
		t.Errorf("a Deployment with no pod: %v", err)
	}
	noAgent := termCluster(t)
	noAgent.agent, noAgent.recorder, noAgent.dyn = nil, nil, nil
	for _, cmd := range []string{"agent scope", "agent why api-1", "get arp"} {
		if _, err := executeKubectl(context.Background(), noAgent, cmd); err == nil {
			t.Errorf("%q without the controller's state: want an error", cmd)
		}
	}
}

func intstrFromInt(n int) intstr.IntOrString { return intstr.FromInt(n) }

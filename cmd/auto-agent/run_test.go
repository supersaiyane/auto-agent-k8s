package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

type agent struct {
	addr   string
	kube   *fake.Clientset
	cancel context.CancelFunc
	done   chan error
}

// boot starts run() against fake clients and waits for OnReady.
func boot(t *testing.T, extra map[string]string) *agent {
	t.Helper()
	dir := t.TempDir()
	env := map[string]string{
		"POD_NAME":               "agent-0",
		"POD_NAMESPACE":          "auto-agent",
		"NODE_NAME":              "node-a",
		"LEADER_LEASE_NAMESPACE": "auto-agent",
		"AUDIT_LOG_PATH":         dir + "/audit.jsonl",
		"LOG_STORE":              "none",
	}
	for k, v := range extra {
		env[k] = v
	}
	conf := config.Load(func(k string) string { return env[k] })
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "autoagent.io", Version: "v1alpha1", Resource: "autoremediationpolicies"}: "AutoRemediationPolicyList",
	})

	a := &agent{addr: freeAddr(t), kube: fake.NewClientset(), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	t.Cleanup(cancel)
	ready := make(chan struct{})
	go func() {
		a.done <- run(ctx, conf, Clients{Kube: a.kube, Dynamic: dyn, HTTP: &http.Client{Timeout: time.Second}},
			RunOptions{HTTPAddr: a.addr, EventsPath: dir + "/events.jsonl", OnReady: func() { close(ready) },
				ForwardEvery: 100 * time.Millisecond})
	}()
	select {
	case <-ready:
	case err := <-a.done:
		t.Fatalf("run returned before ready: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("agent did not become ready")
	}
	return a
}

// stop ends the context and expects a clean, prompt return.
func (a *agent) stop(t *testing.T) {
	t.Helper()
	a.cancel()
	select {
	case err := <-a.done:
		if err != nil {
			t.Fatalf("clean shutdown returns nil, got %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after the context ended")
	}
}

// PLAN-002 9.6: the whole agent boots against fake clients, reports ready,
// and shuts down cleanly when its context ends.
func TestRun_BootsReadyAndShutsDown(t *testing.T) {
	a := boot(t, nil)
	url := fmt.Sprintf("http://%s/readyz", a.addr)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz never returned 200 (last err %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.stop(t)
}

// With short intervals the leader loops run every check against the fake
// API server, with every optional integration configured.
func TestRun_LeaderLoopsRunWithIntegrations(t *testing.T) {
	a := boot(t, map[string]string{
		"SCALE_INTERVAL":   "100ms",
		"JOB_INTERVAL":     "100ms",
		"QUOTA_INTERVAL":   "100ms",
		"HEALTH_INTERVAL":  "100ms",
		"LEARNING_ENABLED": "true",
		"TICKETS_ENABLED":  "true",
		"TICKETS_PROVIDER": "jira",
		"GIT_TOKEN":        "synthetic",
		"GITOPS_REPO":      "o/r",
		"GITOPS_PROVIDER":  "gitlab",
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := a.kube.CoordinationV1().Leases("auto-agent").Get(context.Background(), "auto-agent-leader", metav1.GetOptions{})
		if err == nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "agent-0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the single replica never took the lease (last err %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // several ticks of every loop as leader
	a.stop(t)
}

func TestIntegrationSelection(t *testing.T) {
	load := func(env map[string]string) config.Config { return config.Load(func(k string) string { return env[k] }) }
	if newGitOps(load(nil), nil) != nil {
		t.Fatal("no gitops client without a token and repo")
	}
	if newGitOps(load(map[string]string{"GIT_TOKEN": "x", "GITOPS_REPO": "o/r"}), nil) == nil {
		t.Fatal("github is the default gitops provider")
	}
	if newTicketer(load(nil), nil) != nil {
		t.Fatal("tickets are off by default")
	}
	for _, p := range []string{"jira", "github", "other"} {
		if newTicketer(load(map[string]string{"TICKETS_ENABLED": "true", "TICKETS_PROVIDER": p}), nil) == nil {
			t.Fatalf("provider %q yields a ticketer", p)
		}
	}
	if hostname() == "" {
		t.Fatal("hostname is never empty")
	}
	both := policy.NamespaceSet("a", "b")
	if got := scopeSummary(&policy.Policy{WatchNamespaces: both, FixNamespaces: both, FixCeiling: policy.NamespaceSet("a")}); got != "watch=a,b fix=a anywhere=false" {
		t.Fatalf("scopeSummary: %s", got)
	}
	if got := scopeSummary(&policy.Policy{WatchAll: true}); got != "watch=all non-system namespaces fix= anywhere=false" {
		t.Fatalf("scopeSummary: %s", got)
	}
}

// ADR-001: an unknown role, or a node agent that cannot reach a controller,
// stops at start instead of running half configured.
func TestRun_RejectsBadRoleSettings(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown role":       {"AGENT_ROLE": "everything"},
		"node without link":  {"AGENT_ROLE": "node", "INTERNAL_TOKEN": "x"},
		"node without token": {"AGENT_ROLE": "node", "CONTROLLER_URL": "http://controller"},
	} {
		conf := config.Load(func(k string) string { return env[k] })
		if err := run(context.Background(), conf, Clients{Kube: fake.NewClientset()}, RunOptions{}); err == nil {
			t.Errorf("%s: run must refuse to start", name)
		}
	}
}

// ADR-001, PLAN-002 11.1: a finding made by a node agent appears in the
// controller's event log, and the node agent serves no dashboard.
func TestRun_NodeAgentForwardsToController(t *testing.T) {
	controller := boot(t, map[string]string{"AGENT_ROLE": "controller", "INTERNAL_TOKEN": "node-secret", "DASHBOARD_TOKEN": "dash"})
	node := boot(t, map[string]string{"AGENT_ROLE": "node", "INTERNAL_TOKEN": "node-secret",
		"CONTROLLER_URL": "http://" + controller.addr, "NODE_NAME": "node-a", "POD_NAME": "agent-node-a"})

	if resp, err := http.Get("http://" + node.addr + "/api/status"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a node agent serves no API: %v %v", resp, err)
	}

	ctx := context.Background()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "app", Image: "app:v1"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if _, err := node.kube.CoreV1().Pods("default").Create(ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 5,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	if _, err := node.kube.CoreV1().Pods("default").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		evs := controllerEvents(t, controller.addr, "dash")
		found := false
		for _, e := range evs {
			if e.Reason == "CrashLoopBackOff" && e.Node == "node-a" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node agent's finding never reached the controller; controller has %+v", evs)
		}
		time.Sleep(100 * time.Millisecond)
	}
	node.stop(t)
	controller.stop(t)
}

func controllerEvents(t *testing.T, addr, token string) []events.Event {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/events?limit=100", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []events.Event
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

type fakeElector struct {
	leads bool
	id    string
}

func (f fakeElector) IsLeader() bool { return f.leads }
func (f fakeElector) Leader() string { return f.id }

// ADR-001: the standby finds the leader by its pod name and pod IP, and
// caches the answer until the leader changes.
func TestLeaderTarget(t *testing.T) {
	kc := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "auto-agent"}, Status: corev1.PodStatus{PodIP: "10.1.2.3"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ctrl-new", Namespace: "auto-agent"}},
	)
	for _, tc := range []struct {
		name    string
		el      fakeElector
		want    string
		wantErr bool
	}{
		{"this pod leads", fakeElector{leads: true}, "", false},
		{"no leader yet", fakeElector{}, "", true},
		{"leader found", fakeElector{id: "ctrl-a"}, "http://10.1.2.3:9090", false},
		{"leader has no IP", fakeElector{id: "ctrl-new"}, "", true},
		{"leader pod missing", fakeElector{id: "ctrl-gone"}, "", true},
	} {
		got, err := newLeaderTarget(kc, "auto-agent", "9090", tc.el)()
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: got %q err %v", tc.name, got, err)
		}
	}

	target := newLeaderTarget(kc, "auto-agent", "9090", fakeElector{id: "ctrl-a"})
	if _, err := target(); err != nil {
		t.Fatal(err)
	}
	reads := len(kc.Actions())
	if got, _ := target(); got != "http://10.1.2.3:9090" || len(kc.Actions()) != reads {
		t.Fatal("the same leader is answered from the cache")
	}
	if httpPort(":8080") != "8080" || httpPort("127.0.0.1:1234") != "1234" || httpPort("bad") != "8080" {
		t.Fatal("httpPort")
	}
}

// ISS-059: the leader finds the standby controllers to copy its log to.
func TestPeerResolver(t *testing.T) {
	ctrl := func(name, ip string, phase corev1.PodPhase, app string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "auto-agent", Labels: map[string]string{"app": app}},
			Status: corev1.PodStatus{PodIP: ip, Phase: phase}}
	}
	kc := fake.NewClientset(
		ctrl("ctrl-self", "10.0.0.1", corev1.PodRunning, "auto-agent-controller"),
		ctrl("ctrl-peer", "10.0.0.2", corev1.PodRunning, "auto-agent-controller"),
		ctrl("ctrl-starting", "", corev1.PodPending, "auto-agent-controller"),
		ctrl("ctrl-done", "10.0.0.4", corev1.PodSucceeded, "auto-agent-controller"),
		ctrl("node-agent", "10.0.0.5", corev1.PodRunning, "auto-agent"),
	)
	peers, err := newPeerResolver(kc, "auto-agent", "ctrl-self", "8080")()
	if err != nil || len(peers) != 1 || peers[0] != "http://10.0.0.2:8080" {
		t.Fatalf("peers %v err %v", peers, err)
	}
}

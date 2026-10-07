package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
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
			RunOptions{HTTPAddr: a.addr, EventsPath: dir + "/events.jsonl", OnReady: func() { close(ready) }})
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
	nss := namespaceList(&policy.Policy{NamespaceAllow: map[string]struct{}{"a": {}, "b": {}}})
	if len(nss) != 2 {
		t.Fatalf("namespaceList: %v", nss)
	}
}

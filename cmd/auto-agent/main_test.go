package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
	"github.com/supersaiyane/auto-agent-k8s/internal/slack"
)

func envOf(m map[string]string) config.Getenv { return func(k string) string { return m[k] } }

// ISS-056: version and check-config, which names unknown keys and never
// prints a secret.
func TestCommand(t *testing.T) {
	var out bytes.Buffer
	if code := command([]string{"version"}, envOf(nil), nil, &out); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %d %q", code, out.String())
	}

	env := map[string]string{"AUTO_MODE": "fix", "FIX_NAMESPACES": "payments", "DASHBOARD_TOKEN": "hunter2-token", "SLACK_WEBHOOK_URL": "https://hooks.example.test/x"}
	environ := []string{"AUTO_MODE=fix", "FIX_NAMESPACES=payments", "DASHBOARD_TOKEN=hunter2-token", "PATH=/bin",
		"AUTO_AGENT_SERVICE_HOST=10.0.0.1", "AUTO_AGENT_PORT_8080_TCP=tcp://10.0.0.1:8080", "HOSTNAME=x"}
	out.Reset()
	if code := command([]string{"check-config"}, envOf(env), environ, &out); code != 0 {
		t.Fatalf("clean config: exit %d\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"mode=fix", "fix=", "AUTO_MODE=fix", "DASHBOARD_TOKEN=(set, redacted)",
		"SLACK_WEBHOOK_URL=(set, redacted)", "QUIET_HOURS=(unset: default)", "no unknown keys"} {
		if !strings.Contains(got, want) {
			t.Errorf("check-config lacks %q", want)
		}
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "hooks.example.test") {
		t.Fatal("check-config printed a secret")
	}

	out.Reset()
	if code := command([]string{"check-config"}, envOf(env), append(environ, "AUTO_MOD=fix", "FIX_NAMESPACE=x"), &out); code != 1 ||
		!strings.Contains(out.String(), "unknown keys (not read by the agent): AUTO_MOD, FIX_NAMESPACE") {
		t.Fatalf("misspelled keys: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := command([]string{"bogus"}, envOf(nil), nil, &out); code != 2 || !strings.Contains(out.String(), "usage") {
		t.Fatalf("unknown command: %d %q", code, out.String())
	}
}

// ISS-056: outside a cluster the agent uses the kubeconfig; inside, the
// in-cluster config; any other in-cluster error is returned.
func TestRestConfig(t *testing.T) {
	inCluster := &rest.Config{Host: "https://in-cluster"}
	if rc, err := restConfig(func() (*rest.Config, error) { return inCluster, nil }); err != nil || rc != inCluster {
		t.Fatalf("in cluster: %v %v", rc, err)
	}
	broken := errors.New("token unreadable")
	if _, err := restConfig(func() (*rest.Config, error) { return nil, broken }); !errors.Is(err, broken) {
		t.Fatalf("other errors pass through: %v", err)
	}
	notIn := func() (*rest.Config, error) { return nil, rest.ErrNotInCluster }

	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:6443"}}]
users: [{name: u, user: {}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	if rc, err := restConfig(notIn); err != nil || rc.Host != "https://127.0.0.1:6443" {
		t.Fatalf("kubeconfig fallback: %v %v", rc, err)
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	if _, err := restConfig(notIn); err == nil || !strings.Contains(err.Error(), "no usable kubeconfig") {
		t.Fatalf("no kubeconfig: %v", err)
	}
}

func TestNewClientsAppliesRateLimits(t *testing.T) {
	conf := config.Load(envOf(map[string]string{"KUBE_API_QPS": "7", "KUBE_API_BURST": "9"}))
	rc := &rest.Config{Host: "https://127.0.0.1:6443"}
	cl, err := newClients(rc, conf)
	if err != nil || cl.Kube == nil || cl.Dynamic == nil || rc.QPS != 7 || rc.Burst != 9 {
		t.Fatalf("clients: %v qps=%v burst=%v", err, rc.QPS, rc.Burst)
	}
	if _, err := newClients(&rest.Config{Host: "://bad"}, conf); err == nil {
		t.Fatal("a bad host is an error")
	}
}

func TestSetLogLevel(t *testing.T) {
	for _, level := range []string{"debug", "nonsense"} {
		setLogLevel(level) // must not panic; flag "v" exists only after klog.InitFlags
	}
}

// ISS-055: one start notice per leader term, and a stop notice only from
// the process that leads.
func TestAnnounceLeadership(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posts = append(posts, string(b))
		mu.Unlock()
	}))
	defer hook.Close()
	var leads atomic.Bool
	a := &agent{leads: leads.Load, hr: policy.NewHotReloader(config.Load(envOf(nil)).Policy, "ns", "cm"),
		extras: trackers{slack: slack.New(hook.URL, 2, hook.Client())}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.announceLeadership(ctx, 5*time.Millisecond); close(done) }()
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(posts) }
	time.Sleep(30 * time.Millisecond)
	if count() != 0 || a.leading.Load() {
		t.Fatal("a standby says nothing")
	}
	leads.Store(true)
	time.Sleep(50 * time.Millisecond)
	if count() != 1 || !a.leading.Load() {
		t.Fatalf("becoming leader posts once, got %d", count())
	}
	leads.Store(false)
	time.Sleep(30 * time.Millisecond)
	leads.Store(true)
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	if count() != 2 || !strings.Contains(posts[1], "leading on") {
		t.Fatalf("a new leader term posts again: %v", posts)
	}
}

func TestWaitClosed(t *testing.T) {
	ch := make(chan struct{})
	if waitClosed(ch, 10*time.Millisecond) {
		t.Fatal("an open channel times out")
	}
	close(ch)
	if !waitClosed(ch, time.Second) {
		t.Fatal("a closed channel returns at once")
	}
}

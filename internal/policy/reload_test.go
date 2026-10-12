package policy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func basePolicy() *Policy {
	return &Policy{
		Mode:            Fix,
		WatchNamespaces: NamespaceSet("default"),
		CPUThreshold:    0.8, MaxScaleStep: 2, MaxActionsPer10m: 10,
		MaxReplicas: 50, MinReplicas: 1, CooldownUp: "2m", CooldownDown: "10m", ScaleWindow: "5m",
	}
}

// ISS-026: every valid mode must be reachable through a ConfigMap reload.
// Flipping fix to dry-run is how an operator stops actions without a restart.
func TestReload_AcceptsEveryValidMode(t *testing.T) {
	for _, m := range []Mode{Observe, Suggest, DryRun, Fix} {
		hr := NewHotReloader(basePolicy(), "ns", "cm")
		hr.policy.Mode = Observe
		if m == Observe {
			hr.policy.Mode = Fix
		}
		hr.reload(map[string]string{"AUTO_MODE": string(m)})
		if got := hr.Get().Mode; got != m {
			t.Errorf("reload AUTO_MODE=%s: got mode %s", m, got)
		}
	}
}

// Deps.Policy() hands out snapshots and callers may hold them (ISS-007), so a
// reload must replace the snapshot, never change one already handed out.
func TestReload_DoesNotChangeHeldSnapshot(t *testing.T) {
	hr := NewHotReloader(basePolicy(), "ns", "cm")
	held := hr.Get()
	hr.reload(map[string]string{"AUTO_MODE": string(Observe), "NAMESPACE_ALLOWLIST": "other"})

	if held.Mode != Fix {
		t.Errorf("held snapshot mode changed to %s", held.Mode)
	}
	if !held.Watched("default") || held.Watched("other") {
		t.Errorf("held snapshot watch scope changed: %v", held.WatchNamespaces)
	}
	if hr.Get() == held {
		t.Error("reload returned the same snapshot pointer")
	}
}

// Run with -race: readers and a reloader at the same time.
func TestReload_ConcurrentGetAndReload(t *testing.T) {
	hr := NewHotReloader(basePolicy(), "ns", "cm")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				p := hr.Get()
				_ = p.Mode
				_ = p.Fixable("default")
			}
		}()
	}
	for j := 0; j < 200; j++ {
		mode := Fix
		if j%2 == 0 {
			mode = Observe
		}
		hr.reload(map[string]string{"AUTO_MODE": string(mode)})
	}
	wg.Wait()
}

// PLAN-004 phase 6 (B5): numeric keys reload, a bad number keeps the old
// value, and a value that fails validation rejects the whole reload.
func TestReload_NumericKeys(t *testing.T) {
	hr := NewHotReloader(basePolicy(), "ns", "cm")
	hr.reload(map[string]string{"SCALE_CPU_THRESHOLD": "0.6", "MAX_SCALE_STEP": "3", "MAX_ACTIONS_PER_10M": "20",
		"MAX_REPLICAS": "30", "MIN_REPLICAS": "2", "COOLDOWN_UP": "1m", "COOLDOWN_DOWN": "5m"})
	p := hr.Get()
	if p.CPUThreshold != 0.6 || p.MaxScaleStep != 3 || p.MaxActionsPer10m != 20 || p.MaxReplicas != 30 || p.MinReplicas != 2 ||
		p.CooldownUp != "1m" || p.CooldownDown != "5m" {
		t.Fatalf("numeric keys not applied: %+v", p)
	}
	hr.reload(map[string]string{"SCALE_CPU_THRESHOLD": "high", "MAX_SCALE_STEP": "many"})
	if p := hr.Get(); p.CPUThreshold != 0.6 || p.MaxScaleStep != 3 {
		t.Fatalf("an unparseable number keeps the current value: %+v", p)
	}
	for _, bad := range []map[string]string{{"MAX_SCALE_STEP": "0", "AUTO_MODE": "dry-run"}, {"MAX_ACTIONS_PER_10M": "0", "AUTO_MODE": "dry-run"}} {
		hr.reload(bad)
		if p := hr.Get(); p.Mode != Fix || p.MaxScaleStep != 3 || p.MaxActionsPer10m != 20 {
			t.Fatalf("an invalid value rejects the whole reload, %v gave %+v", bad, p)
		}
	}
}

func TestHotReloader_FollowsTheConfigMapAndTheScope(t *testing.T) {
	pol := basePolicy()
	pol.WatchNamespaces = NamespaceSet("default", "other") // the ceiling is always inside the watch scope
	pol.FixCeiling = NamespaceSet("default", "other")
	pol.FixNamespaces = NamespaceSet("other")
	scope := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ScopeConfigMap, Namespace: "ns"}, Data: map[string]string{ScopeKey: "default"}}
	settings := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"}, Data: map[string]string{"AUTO_MODE": "fix"}}
	kc := fake.NewSimpleClientset(scope, settings)
	hr := NewHotReloader(pol, "ns", "cm")
	hr.WatchScope(ScopeConfigMap)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hr.Start(ctx, kc)

	eventually(t, "the dashboard choice applies", func() bool { return strings.Join(hr.Get().FixScope(), ",") == "default" })

	other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "ns"}, Data: map[string]string{"AUTO_MODE": "observe"}}
	if _, err := kc.CoreV1().ConfigMaps("ns").Create(ctx, other, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	other.Data["AUTO_MODE"] = "suggest"
	if _, err := kc.CoreV1().ConfigMaps("ns").Update(ctx, other, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	settings.Data["AUTO_MODE"] = "dry-run"
	if _, err := kc.CoreV1().ConfigMaps("ns").Update(ctx, settings, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "an update to the agent's ConfigMap reloads", func() bool { return hr.Get().Mode == DryRun })

	if err := kc.CoreV1().ConfigMaps("ns").Delete(ctx, ScopeConfigMap, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleting the choice returns to FIX_NAMESPACES", func() bool { return strings.Join(hr.Get().FixScope(), ",") == "other" })
}

func TestHotReloader_NoScopeConfigMapMeansTheHelmList(t *testing.T) {
	pol := basePolicy()
	pol.WatchNamespaces, pol.FixCeiling, pol.FixNamespaces = NamespaceSet("other"), NamespaceSet("other"), NamespaceSet("other")
	hr := NewHotReloader(pol, "ns", "cm")
	hr.WatchScope(ScopeConfigMap)
	if len(hr.Get().FixScope()) != 0 {
		t.Fatal("until the scope is read, the agent fixes nowhere")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hr.Start(ctx, fake.NewSimpleClientset())
	if strings.Join(hr.Get().FixScope(), ",") != "other" {
		t.Fatalf("with no dashboard choice FIX_NAMESPACES applies: %v", hr.Get().FixScope())
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

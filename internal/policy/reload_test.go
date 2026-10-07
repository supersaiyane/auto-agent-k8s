package policy

import (
	"sync"
	"testing"
)

func basePolicy() *Policy {
	return &Policy{
		Mode:           Fix,
		NamespaceAllow: map[string]struct{}{"default": {}},
		CPUThreshold:   0.8, MaxScaleStep: 2, MaxActionsPer10m: 10,
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
	if _, ok := held.NamespaceAllow["default"]; !ok || len(held.NamespaceAllow) != 1 {
		t.Errorf("held snapshot allowlist changed: %v", held.NamespaceAllow)
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
				_ = p.AllowedNamespace("default")
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

package policy

import (
	"strings"
	"testing"
)

// ADR-002: the watch scope, the ceiling and the fix scope, from the
// environment.
func TestScope_Load(t *testing.T) {
	for _, tc := range []struct {
		name                string
		env                 map[string]string
		watched, notWatched []string
		ceiling             []string
		fix                 string
	}{
		{"defaults", map[string]string{"POD_NAMESPACE": "auto-agent"},
			[]string{"default", "payments"}, []string{"kube-system", "kube-public", "kube-node-lease", "auto-agent", ""}, nil, ""},
		{"star and all mean every namespace", map[string]string{"WATCH_NAMESPACES": " * "},
			[]string{"payments"}, []string{"kube-system"}, nil, ""},
		{"explicit watch list wins over the system list", map[string]string{"WATCH_NAMESPACES": "a, kube-system"},
			[]string{"a", "kube-system"}, []string{"b"}, nil, ""},
		{"ceiling defaults to the fix list", map[string]string{"FIX_NAMESPACES": "b,a"},
			[]string{"a"}, nil, []string{"a", "b"}, "a,b"},
		{"fix list outside the ceiling is dropped", map[string]string{"FIX_NAMESPACES": "a,b", "FIX_CEILING": "a,c"},
			nil, nil, []string{"a", "c"}, "a"},
		{"fix list outside the watch scope is dropped", map[string]string{"WATCH_NAMESPACES": "a", "FIX_NAMESPACES": "a,b"},
			nil, []string{"b"}, []string{"a"}, "a"},
		{"fix anywhere excludes only system namespaces", map[string]string{"FIX_ANYWHERE": "true", "FIX_NAMESPACES": "x,kube-system", "POD_NAMESPACE": "me"},
			nil, []string{"me"}, []string{"x", "anything"}, "x"},
		{"deprecated alias", map[string]string{"NAMESPACE_ALLOWLIST": "p,q"},
			[]string{"p", "q"}, []string{"default"}, []string{"p", "q"}, "p,q"},
		{"new keys win over the alias", map[string]string{"NAMESPACE_ALLOWLIST": "p", "WATCH_NAMESPACES": "all", "FIX_NAMESPACES": "r"},
			[]string{"default"}, nil, []string{"r"}, "r"},
	} {
		p := Load(env(tc.env))
		for _, ns := range tc.watched {
			if !p.Watched(ns) {
				t.Errorf("%s: %q should be watched", tc.name, ns)
			}
		}
		for _, ns := range tc.notWatched {
			if p.Watched(ns) || p.InCeiling(ns) || p.Fixable(ns) {
				t.Errorf("%s: %q should be neither watched nor fixable", tc.name, ns)
			}
		}
		for _, ns := range tc.ceiling {
			if !p.InCeiling(ns) {
				t.Errorf("%s: %q should be inside the ceiling", tc.name, ns)
			}
		}
		if got := strings.Join(p.FixScope(), ","); got != tc.fix {
			t.Errorf("%s: fix scope %q, want %q", tc.name, got, tc.fix)
		}
	}
}

// The dashboard choice replaces FIX_NAMESPACES but never leaves the
// ceiling, and building it never changes a snapshot already handed out.
func TestScope_FixOverride(t *testing.T) {
	p := Load(env(map[string]string{"FIX_NAMESPACES": "a", "FIX_CEILING": "a,b"}))
	o := p.WithFixOverride([]string{"b", "outside"})
	if !o.Fixable("b") || o.Fixable("a") || o.Fixable("outside") || strings.Join(o.FixScope(), ",") != "b" {
		t.Fatalf("override scope: %v", o.FixScope())
	}
	if !p.Fixable("a") || p.Fixable("b") || p.FixOverride != nil {
		t.Fatal("WithFixOverride changed the receiver")
	}
	if none := p.WithFixOverride([]string{}); len(none.FixScope()) != 0 || none.FixOverride == nil {
		t.Fatal("an empty choice fixes nowhere, and differs from no choice")
	}
	if back := o.WithFixOverride(nil); strings.Join(back.FixScope(), ",") != "a" {
		t.Fatalf("clearing the choice returns to FIX_NAMESPACES: %v", back.FixScope())
	}
}

func TestScope_WatchList(t *testing.T) {
	if names, all := Load(env(map[string]string{"WATCH_NAMESPACES": "b,a"})).WatchList(); all || strings.Join(names, ",") != "a,b" {
		t.Fatalf("explicit list: %v %v", names, all)
	}
	if names, all := Load(env(nil)).WatchList(); !all || names != nil {
		t.Fatalf("watch all: %v %v", names, all)
	}
}

func TestScope_ValidateRejectsEmptyWatchList(t *testing.T) {
	p := basePolicy()
	p.WatchNamespaces = NamespaceSet(" ", "")
	if err := p.Validate(); err == nil {
		t.Fatal("an explicit watch list naming nothing is rejected")
	}
}

// A ConfigMap reload changes the watch scope and the fix list (a present key
// wins even when empty), and never the ceiling, which mirrors the RBAC.
func TestScope_Reload(t *testing.T) {
	hr := NewHotReloader(Load(env(map[string]string{"WATCH_NAMESPACES": "a,b", "FIX_NAMESPACES": "a", "FIX_CEILING": "a,b"})), "ns", "cm")
	hr.reload(map[string]string{"FIX_NAMESPACES": "b", "FIX_CEILING": "c", "FIX_ANYWHERE": "true"})
	if p := hr.Get(); !p.Fixable("b") || p.Fixable("a") || p.InCeiling("c") || p.FixAnywhere {
		t.Fatalf("fix list reloads, ceiling does not: %v", p.FixScope())
	}
	hr.reload(map[string]string{"FIX_NAMESPACES": ""})
	if len(hr.Get().FixScope()) != 0 {
		t.Fatal("an empty FIX_NAMESPACES clears the fix list")
	}
	hr.reload(map[string]string{"WATCH_NAMESPACES": ""})
	if p := hr.Get(); !p.Watched("other") {
		t.Fatal("an empty WATCH_NAMESPACES watches every namespace")
	}
	hr.reload(map[string]string{"WATCH_NAMESPACES": "a"})
	if p := hr.Get(); p.Watched("other") || !p.Watched("a") {
		t.Fatal("a watch list reloads")
	}
	hr.reload(map[string]string{"NAMESPACE_ALLOWLIST": "z"})
	if p := hr.Get(); !p.Watched("z") || p.Fixable("z") {
		t.Fatal("the alias reloads both lists, still inside the ceiling")
	}
	hr.reload(map[string]string{"AUTO_MODE": "observe"})
	if !hr.Get().Watched("z") {
		t.Fatal("a reload without scope keys keeps the scope")
	}
}

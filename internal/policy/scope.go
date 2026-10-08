package policy

import (
	"sort"
	"strings"

	"k8s.io/klog/v2"
)

// Namespace scopes (ADR-002). The watch scope decides where the agent reads,
// detects and reports. The fix ceiling is where the chart granted write
// permissions. The fix scope is where the agent may act today: the dashboard
// choice when one exists, else FIX_NAMESPACES, always inside the ceiling.

// systemNamespaces are never watched by default and never inside the ceiling
// in fix-anywhere mode. The agent's own namespace is added at load.
func systemNamespaces() []string { return []string{"kube-system", "kube-public", "kube-node-lease"} }

// nsSet is a set of namespace names.
type nsSet map[string]struct{}

func (s nsSet) has(ns string) bool { _, ok := s[ns]; return ok }

func (s nsSet) sorted() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NamespaceSet builds a set from names, ignoring blanks. Tests and callers
// outside this package use it to fill the scope fields.
func NamespaceSet(names ...string) map[string]struct{} {
	s := nsSet{}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			s[n] = struct{}{}
		}
	}
	return s
}

func parseNamespaceList(s string) map[string]struct{} {
	return NamespaceSet(strings.Split(s, ",")...)
}

// isAll reports whether a WATCH_NAMESPACES value means every namespace.
func isAll(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == "*" || strings.EqualFold(v, "all")
}

// loadScope fills the scope fields from the environment. NAMESPACE_ALLOWLIST
// is a deprecated alias for both the watch scope and the initial fix scope.
func loadScope(p *Policy, g envReader) {
	p.System = NamespaceSet(append(systemNamespaces(), g.str("POD_NAMESPACE", ""))...)
	legacy := g.str("NAMESPACE_ALLOWLIST", "")
	if legacy != "" {
		klog.Warning("policy: NAMESPACE_ALLOWLIST is deprecated; use WATCH_NAMESPACES and FIX_NAMESPACES (ADR-002)")
	}
	watch := g.str("WATCH_NAMESPACES", legacy)
	p.WatchAll = isAll(watch)
	if !p.WatchAll {
		p.WatchNamespaces = parseNamespaceList(watch)
	}
	fix := g.str("FIX_NAMESPACES", legacy)
	p.FixNamespaces = parseNamespaceList(fix)
	p.FixCeiling = parseNamespaceList(g.str("FIX_CEILING", fix))
	p.FixAnywhere = g.bool("FIX_ANYWHERE", false)
	if p.FixAnywhere {
		klog.Warning("policy: FIX_ANYWHERE is on: the dashboard can enable fixing in any non-system namespace, and a leaked token can disrupt any of them")
	}
	for _, ns := range nsSet(p.FixNamespaces).sorted() {
		if !p.InCeiling(ns) {
			klog.Warningf("policy: FIX_NAMESPACES names %q, which is outside the fix ceiling or the watch scope; the agent will not act there", ns)
		}
	}
}

// Watched reports whether the agent reads and reports in ns. Cluster-scoped
// objects (ns "") are not namespaced and are never filtered by this call.
func (p *Policy) Watched(ns string) bool {
	if ns == "" {
		return false
	}
	if p.WatchAll {
		return !nsSet(p.System).has(ns)
	}
	return nsSet(p.WatchNamespaces).has(ns)
}

// InCeiling reports whether the dashboard may enable fixing in ns: the chart
// granted writes there (or fix-anywhere is on) and the namespace is watched.
func (p *Policy) InCeiling(ns string) bool {
	if !p.Watched(ns) {
		return false
	}
	if p.FixAnywhere {
		return !nsSet(p.System).has(ns)
	}
	return nsSet(p.FixCeiling).has(ns)
}

// Fixable reports whether the agent may change objects in ns today.
func (p *Policy) Fixable(ns string) bool {
	return p.InCeiling(ns) && nsSet(p.fixChoice()).has(ns)
}

// fixChoice is the dashboard choice when one exists, else FIX_NAMESPACES.
func (p *Policy) fixChoice() map[string]struct{} {
	if p.FixOverride != nil {
		return p.FixOverride
	}
	return p.FixNamespaces
}

// FixScope lists the namespaces the agent may act in today, sorted.
func (p *Policy) FixScope() []string {
	out := []string{}
	for _, ns := range nsSet(p.fixChoice()).sorted() {
		if p.InCeiling(ns) {
			out = append(out, ns)
		}
	}
	return out
}

// WatchList returns the explicit watch list, sorted, and false; or nil and
// true when every non-system namespace is watched.
func (p *Policy) WatchList() ([]string, bool) {
	if p.WatchAll {
		return nil, true
	}
	return nsSet(p.WatchNamespaces).sorted(), false
}

// WithFixOverride returns a copy of p whose fix scope is the dashboard
// choice names; nil clears the choice and returns to FIX_NAMESPACES. The
// receiver is never changed, so a held snapshot stays as it was.
func (p *Policy) WithFixOverride(names []string) *Policy {
	c := *p
	if names == nil {
		c.FixOverride = nil
	} else {
		c.FixOverride = NamespaceSet(names...)
	}
	return &c
}

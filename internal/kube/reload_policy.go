package kube

import "strings"

// Config reload, part 2 (PLAN-002 A1.3): which changes reload which
// workloads. Stakater Reloader's annotations are read as they are, so
// workloads migrate without edits; auto-agent.io names work alongside.
const (
	annReload           = "auto-agent.io/reload"            // "false" turns reload off for a workload or object
	annReloadConfigMaps = "auto-agent.io/reload-configmaps" // comma list: reload on any change to these
	annReloadSecrets    = "auto-agent.io/reload-secrets"    // comma list
	annReloadOn         = "auto-agent.io/reload-on"         // auto or always
	annConfigHash       = "auto-agent.io/config-hash"       // pod template annotation the reload patches

	annStakaterAuto      = "reloader.stakater.com/auto"
	annStakaterCMAuto    = "configmap.reloader.stakater.com/auto"
	annStakaterSecAuto   = "secret.reloader.stakater.com/auto"
	annStakaterCMReload  = "configmap.reloader.stakater.com/reload"
	annStakaterSecReload = "secret.reloader.stakater.com/reload"
	annStakaterSearch    = "reloader.stakater.com/search"
	annStakaterMatch     = "reloader.stakater.com/match"
	annStakaterIgnore    = "reloader.stakater.com/ignore"
)

// Reload modes for reload.reloadOn and auto-agent.io/reload-on.
const (
	reloadOnAuto   = "auto"   // restart only when a running pod cannot see the change
	reloadOnAlways = "always" // restart on every change to a used key
)

// reloadRules are a workload's reload annotations.
type reloadRules struct {
	off    map[refKind]bool // reload turned off, per kind
	named  map[objRef]bool  // reload on any change to these, used or not
	search bool             // only objects annotated match=true count
	on     string           // auto, always, or "" for the default
}

func workloadRules(ann map[string]string) reloadRules {
	r := reloadRules{off: map[refKind]bool{}, named: map[objRef]bool{}, on: ann[annReloadOn]}
	if isFalse(ann[annReload]) || isFalse(ann[annStakaterAuto]) {
		r.off[refConfigMap], r.off[refSecret] = true, true
	}
	if isFalse(ann[annStakaterCMAuto]) {
		r.off[refConfigMap] = true
	}
	if isFalse(ann[annStakaterSecAuto]) {
		r.off[refSecret] = true
	}
	for _, kv := range []struct {
		key  string
		kind refKind
	}{{annReloadConfigMaps, refConfigMap}, {annStakaterCMReload, refConfigMap}, {annReloadSecrets, refSecret}, {annStakaterSecReload, refSecret}} {
		for _, name := range strings.Split(ann[kv.key], ",") {
			if name = strings.TrimSpace(name); name != "" {
				r.named[objRef{kv.kind, name}] = true
			}
		}
	}
	r.search = isTrue(ann[annStakaterSearch])
	return r
}

// objectIgnored reports whether a ConfigMap or Secret opts out of reloads.
func objectIgnored(ann map[string]string) bool {
	return isTrue(ann[annStakaterIgnore]) || isFalse(ann[annReload])
}

// shouldReload decides whether a change to ref, touching the changed keys,
// reloads a workload with these rules that uses ref as use (nil: unused).
// defaultOn is reload.reloadOn.
func shouldReload(r reloadRules, ref objRef, use *refUse, changed []string, objAnn map[string]string, defaultOn string) bool {
	if r.off[ref.Kind] || objectIgnored(objAnn) || len(changed) == 0 {
		return false
	}
	if r.named[ref] {
		return true // asked for by name: any change restarts it
	}
	if use == nil || (r.search && !isTrue(objAnn[annStakaterMatch])) || !use.affects(changed) {
		return false
	}
	on := r.on
	if on != reloadOnAlways && on != reloadOnAuto {
		on = defaultOn
	}
	return on == reloadOnAlways || use.NeedsRestart
}

func isTrue(v string) bool  { return strings.EqualFold(strings.TrimSpace(v), "true") }
func isFalse(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "false") }

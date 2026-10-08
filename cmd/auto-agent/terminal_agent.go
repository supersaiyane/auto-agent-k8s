package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpapi"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
)

// termAgent answers the dashboard terminal's `agent` commands from the
// agent's own state (PLAN-003 phase 3).
type termAgent struct{ a *agent }

func (t termAgent) Scope() string {
	pol := t.a.hr.Get()
	watch := "every namespace except the system ones"
	if names, all := pol.WatchList(); !all {
		watch = strings.Join(names, ", ")
	}
	ceiling := keys(pol.FixCeiling)
	if pol.FixAnywhere {
		ceiling = "every non-system namespace (rbac.fixAnywhere: a leaked token can disrupt any namespace)"
	}
	source := "Helm (agent.fixNamespaces)"
	if pol.FixOverride != nil {
		source = "the Settings tab"
	}
	return fmt.Sprintf("Watch scope:  %s\nFix scope:    %s (from %s)\nFix ceiling:  %s\nMode:         %s",
		watch, orNowhere(strings.Join(pol.FixScope(), ", ")), source, orNowhere(ceiling), pol.Mode)
}

func (t termAgent) Status() string {
	return fmt.Sprintf("Version:  %s\nRole:     %s\nPod:      %s on %s\nLeader:   %v\nMode:     %s",
		version, t.a.conf.Role, t.a.conf.PodName, t.a.conf.NodeName, t.a.isLeader(), t.a.hr.Get().Mode)
}

func (t termAgent) Gate(ns string) []httpapi.GateRow {
	var rows []httpapi.GateRow
	for _, c := range kube.GatePreview(t.a.deps, ns) {
		rows = append(rows, httpapi.GateRow{Check: c.Check, Pass: c.Pass, Detail: c.Detail})
	}
	return rows
}

func keys(m map[string]struct{}) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func orNowhere(s string) string {
	if s == "" {
		return "nowhere"
	}
	return s
}

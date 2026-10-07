package kube

import (
	"strings"
	"testing"
)

// PLAN-002 10.5 and 10.11: one case per cause, from real scheduler wording.
func TestParseSchedulingFailure_OneCasePerCause(t *testing.T) {
	for _, tc := range []struct {
		name, msg, kind, text string
		nodes                 int
	}{
		{"taint", "0/1 nodes are available: 1 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }.",
			"taint", "had untolerated taint {node-role.kubernetes.io/control-plane: }", 1},
		{"node affinity", "0/2 nodes are available: 2 node(s) didn't match Pod's node affinity/selector.",
			"affinity", "didn't match Pod's node affinity/selector", 2},
		{"pod anti-affinity", "0/3 nodes are available: 3 node(s) didn't match pod anti-affinity rules.",
			"affinity", "didn't match pod anti-affinity rules", 3},
		{"cpu", "0/4 nodes are available: 4 Insufficient cpu.", "resources", "Insufficient cpu", 4},
		{"pod count", "0/1 nodes are available: 1 Too many pods.", "resources", "Too many pods", 1},
		{"unbound claim", "0/3 nodes are available: pod has unbound immediate PersistentVolumeClaims.",
			"volume", "pod has unbound immediate PersistentVolumeClaims", 0},
		{"zonal volume", "0/2 nodes are available: 2 node(s) had volume node affinity conflict.",
			"volume", "had volume node affinity conflict", 2},
		{"topology spread", "0/3 nodes are available: 3 node(s) didn't match pod topology spread constraints.",
			"topology", "didn't match pod topology spread constraints", 3},
		{"unknown", "0/1 nodes are available: 1 node(s) were unschedulable.", "other", "were unschedulable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSchedulingFailure(tc.msg)
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].Text != tc.text || got[0].Nodes != tc.nodes {
				t.Fatalf("got %+v, want kind=%s nodes=%d text=%q", got, tc.kind, tc.nodes, tc.text)
			}
			if schedFix(tc.kind) == "" {
				t.Fatal("every kind has guidance")
			}
		})
	}
}

func TestParseSchedulingFailure_SeveralCausesAndPreemptionTail(t *testing.T) {
	msg := "0/3 nodes are available: 1 node(s) had untolerated taint {a: b, c: d}, 2 Insufficient memory. " +
		"preemption: 0/3 nodes are available: 1 Preemption is not helpful for scheduling, 2 No preemption victims found."
	got := parseSchedulingFailure(msg)
	if len(got) != 2 || got[0].Kind != "taint" || got[0].Text != "had untolerated taint {a: b, c: d}" ||
		got[1].Kind != "resources" || got[1].Nodes != 2 {
		t.Fatalf("got %+v", got)
	}
	if parseSchedulingFailure("") != nil || parseSchedulingFailure("0/0 nodes are available: ") != nil {
		t.Fatal("no causes in an empty message")
	}
	if c := parseSchedulingFailure("something else, entirely"); len(c) != 2 || c[0].Kind != "other" ||
		!strings.Contains(c[1].Text, "entirely") {
		t.Fatalf("unknown wording is kept as other: %+v", c)
	}
}

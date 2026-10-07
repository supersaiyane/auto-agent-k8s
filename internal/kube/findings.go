package kube

import (
	"context"
	"fmt"
	"strings"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// Rung is how far the agent may go on a finding (PLAN-002 Part C, the fix
// ladder). Every detector added in phase 10 states its rung.
type Rung string

const (
	RungAlert   Rung = "R0" // tell people, nothing more
	RungGuided  Rung = "R1" // name the cause and the exact fix; a person applies it
	RungPR      Rung = "R2" // open a pull request with the fix
	RungApprove Rung = "R3" // apply the fix after a named person approves
	RungAuto    Rung = "R4" // apply the fix through the mutation gate
)

var rungNames = map[Rung]string{
	RungAlert:   "alert only",
	RungGuided:  "guided fix",
	RungPR:      "pull request",
	RungApprove: "approve to fix",
	RungAuto:    "automatic fix",
}

// finding is one detected problem, reported the same way by every detector.
type finding struct {
	Reason    string
	Namespace string
	Workload  string
	Pod       string
	Node      string
	Severity  eventsvc.Severity
	Rung      Rung
	// Target is the rung this finding reaches once the approval queue lands
	// (PLAN-002 phase 14); empty when Rung is already final.
	Target  Rung
	Summary string   // one line: what is wrong
	Details []string // facts that support it
	Fix     string   // what a person should do
	// Subject separates findings that share a workload, for example two
	// volumes on one pod; it only feeds the dedup key.
	Subject string
}

func (f finding) message() string {
	var b strings.Builder
	where := f.Namespace + "/" + f.Workload
	if f.Pod != "" && f.Pod != f.Workload {
		where += " (pod " + f.Pod + ")"
	}
	fmt.Fprintf(&b, "*%s* `%s`: %s\n", f.Reason, where, f.Summary)
	for _, d := range f.Details {
		fmt.Fprintf(&b, "%s\n", d)
	}
	if f.Fix != "" {
		fmt.Fprintf(&b, "_Fix_: %s\n", f.Fix)
	}
	fmt.Fprintf(&b, "_Rung %s, %s_", f.Rung, rungNames[f.Rung])
	if f.Target != "" {
		fmt.Fprintf(&b, "; %s %s arrives with the approval queue", f.Target, rungNames[f.Target])
	}
	b.WriteString("\n")
	return b.String()
}

// report sends a finding once per dedup window: Slack, Alertmanager, the
// event log (with its rung) and the incident counter. It returns false when
// the finding was suppressed as a repeat.
func report(ctx context.Context, deps *Deps, f finding) bool {
	if !deps.Dedup.Check(dedupKey(f.Namespace, f.Workload+"/"+f.Subject, f.Reason)) {
		return false
	}
	msg := f.message()
	if err := deps.Slack.Post(msg); err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("finding", "slack").Inc()
	}
	fireAlert(ctx, deps, f.Reason, f.Namespace, f.Workload, f.Pod, msg, string(f.Severity))
	recordEvent(deps, eventsvc.Event{
		Type: eventsvc.Incident, Severity: f.Severity,
		Namespace: f.Namespace, Workload: f.Workload, Pod: f.Pod, Node: f.Node,
		Reason: f.Reason, Message: f.Summary, Rung: string(f.Rung),
	})
	obs.IncidentsTotal.WithLabelValues(f.Reason, f.Namespace, f.Workload).Inc()
	return true
}

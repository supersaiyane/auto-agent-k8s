package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"text/tabwriter"
)

// The `agent` commands (PLAN-003 phase 3) answer questions about the agent
// itself, from its own state, without acting.

func agentOf(e *termEnv) (TerminalAgent, error) {
	if e.agent == nil {
		return nil, fmt.Errorf("agent commands are answered by the controller")
	}
	return e.agent, nil
}

func runAgentScope(_ context.Context, e *termEnv, _ termArgs) (string, error) {
	ag, err := agentOf(e)
	if err != nil {
		return "", err
	}
	return ag.Scope(), nil
}

func runAgentStatus(_ context.Context, e *termEnv, _ termArgs) (string, error) {
	ag, err := agentOf(e)
	if err != nil {
		return "", err
	}
	return ag.Status(), nil
}

// runAgentGate says whether a fix in the namespace would pass now, check by
// check, without spending any budget (3.3).
func runAgentGate(_ context.Context, e *termEnv, a termArgs) (string, error) {
	ag, err := agentOf(e)
	if err != nil {
		return "", err
	}
	rows := ag.Gate(a.ns)
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CHECK\tRESULT\tDETAIL")
	pass := true
	for _, r := range rows {
		res := "pass"
		if !r.Pass {
			res, pass = "BLOCKS", false
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", r.Check, res, r.Detail)
	}
	w.Flush()
	verdict := "A fix in " + a.ns + " would pass the gate now."
	if !pass {
		verdict = "A fix in " + a.ns + " would not be applied now."
	}
	return buf.String() + "\n" + verdict, nil
}

// runAgentWhy lists every finding, gate decision and fix for one pod, from
// the event log, oldest first (3.2).
func runAgentWhy(_ context.Context, e *termEnv, a termArgs) (string, error) {
	if len(a.pos) == 0 {
		return "", fmt.Errorf("usage: agent why <pod> [-n <ns>]")
	}
	if e.recorder == nil {
		return "", fmt.Errorf("the event log is kept by the controller")
	}
	pod := a.pos[0]
	evs := e.recorder.Recent(0)
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tTYPE\tREASON\tRESULT\tDETAIL")
	n := 0
	for i := len(evs) - 1; i >= 0; i-- { // Recent is newest first
		ev := evs[i]
		if ev.Namespace != a.ns || ev.Pod != pod {
			continue
		}
		n++
		result := ev.Result
		if result == "" {
			result = ev.Action
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ev.Timestamp.UTC().Format("2006-01-02 15:04:05"), ev.Type, ev.Reason, result, trunc(ev.Message, 100))
	}
	w.Flush()
	if n == 0 {
		return fmt.Sprintf("the agent has recorded nothing about pod %s in %s", pod, a.ns), nil
	}
	return buf.String(), nil
}

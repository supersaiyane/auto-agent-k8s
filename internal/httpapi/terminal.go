package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/redact"
)

// The dashboard terminal (PLAN-003): a read-only kubectl. One command table
// decides what runs, and the same table produces `help`, the panel served
// at /api/kubectl/help and the refusals, so they cannot drift. Every change
// to the cluster goes through the mutation gate; the terminal never writes.

// maxTermOutput caps one answer, so a huge log cannot stall the browser.
const maxTermOutput = 256 << 10

type kubectlRequest struct {
	Command string `json:"command"`
}

type kubectlResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// TerminalAgent answers the terminal's `agent` commands (PLAN-003 phase 3);
// cmd/auto-agent builds it from the agent's own state.
type TerminalAgent interface {
	Scope() string            // watch scope, fix scope, ceiling, fix-anywhere
	Gate(ns string) []GateRow // would a fix in ns pass now, check by check
	Status() string           // role, leader, mode, version
}

// GateRow is one guardrail's answer for `agent gate`.
type GateRow struct {
	Check  string `json:"check"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// termEnv is what a command can read.
type termEnv struct {
	kc       kubernetes.Interface
	dyn      dynamic.Interface
	allowNS  func(string) bool
	agent    TerminalAgent
	recorder *events.Recorder
	now      time.Time
}

// termArgs is a parsed command line.
type termArgs struct {
	verb, sub     string   // "get pods" -> get, pods; "rollout status" -> rollout, status
	pos           []string // positional arguments after the sub
	ns            string
	allNs         bool
	container     string
	tail          int64
	previous      bool
	since         time.Duration
	output        string
	selector      string
	fieldSelector string
	forObj        string
}

// termCmd is one row of the command table.
type termCmd struct {
	Verb    string   `json:"verb"`
	Sub     string   `json:"sub,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Usage   string   `json:"usage"`
	About   string   `json:"about"`
	Cluster bool     `json:"cluster"` // reads no namespaced data
	run     func(ctx context.Context, e *termEnv, a termArgs) (string, error)
}

// refusal is a command that is never added, and why.
type refusal struct {
	Commands string `json:"commands"`
	Why      string `json:"why"`
}

// neverAdded are refused with their reason (PLAN-003 "Never added").
func neverAdded() []refusal {
	return []refusal{
		{"delete, apply, create, replace, edit, patch, scale, label, annotate, taint, set, cordon, uncordon, drain, rollout restart, rollout undo, rollout pause, rollout resume",
			"writes would bypass the mutation gate, its guardrails and its audit trail; fixes come from the agent, and from approvals in PLAN-002 phase 15"},
		{"exec, attach, cp, debug, port-forward, proxy, run", "shell or network access into workloads for anyone holding the dashboard token"},
		{"get secrets, describe secret, ConfigMap values", "credentials; ConfigMaps show key names only"},
		{"anything outside the watch scope", "reads follow the watch scope (ADR-002)"},
	}
}

// refusedVerbs maps a refused first word (or "rollout <sub>") to its reason.
func refusedVerbs() map[string]string {
	out := map[string]string{}
	for _, r := range neverAdded()[:2] {
		for _, c := range strings.Split(r.Commands, ", ") {
			out[c] = r.Why
		}
	}
	return out
}

// termCommands is the command table.
func termCommands() []termCmd {
	ns := "[-n <ns> | -A] [-l <selector>] [--field-selector <selector>] [-o wide]"
	cmds := []termCmd{}
	for _, g := range getKinds() {
		cmds = append(cmds, termCmd{Verb: "get", Sub: g.name, Aliases: g.aliases, Cluster: g.cluster,
			Usage: "get " + g.name + " [name] " + map[bool]string{true: "[-l <selector>] [-o wide]", false: ns}[g.cluster],
			About: g.about, run: g.run})
	}
	for _, d := range describeKinds() {
		cmds = append(cmds, termCmd{Verb: "describe", Sub: d.name, Aliases: d.aliases, Cluster: d.cluster,
			Usage: "describe " + d.name + " <name>" + map[bool]string{true: "", false: " [-n <ns>]"}[d.cluster], About: d.about, run: d.run})
	}
	return append(cmds,
		termCmd{Verb: "logs", Usage: "logs <pod> | deploy/<name> [-n <ns>] [-c <container>] [--tail <n>] [--previous] [--since <duration>]",
			About: "container logs; deploy/<name> picks a pod of the Deployment", run: runLogs},
		termCmd{Verb: "events", Usage: "events [-n <ns> | -A] [--for <kind>/<name>]", About: "events, newest last", run: runEvents},
		termCmd{Verb: "rollout", Sub: "status", Usage: "rollout status deploy/<name> | sts/<name> [-n <ns>]", About: "progress of a rollout", run: runRolloutStatus},
		termCmd{Verb: "rollout", Sub: "history", Usage: "rollout history deploy/<name> | sts/<name> [-n <ns>]", About: "revisions and their images", run: runRolloutHistory},
		termCmd{Verb: "auth", Sub: "can-i", Usage: "auth can-i <verb> <resource> [-n <ns>]", About: "what the agent itself may do (SelfSubjectAccessReview)", Cluster: true, run: runCanI},
		termCmd{Verb: "agent", Sub: "scope", Usage: "agent scope", About: "watch scope, fix scope, ceiling and fix-anywhere", Cluster: true, run: runAgentScope},
		termCmd{Verb: "agent", Sub: "why", Usage: "agent why <pod> [-n <ns>]", About: "every finding, gate decision and fix for one pod", run: runAgentWhy},
		termCmd{Verb: "agent", Sub: "gate", Usage: "agent gate [-n <ns>]", About: "would a fix in this namespace pass now, without acting", run: runAgentGate},
		termCmd{Verb: "agent", Sub: "status", Usage: "agent status", About: "role, leader, mode, version", Cluster: true, run: runAgentStatus},
		termCmd{Verb: "version", Usage: "version", About: "API server version", Cluster: true, run: runVersion},
		termCmd{Verb: "help", Usage: "help", About: "this list", Cluster: true, run: func(context.Context, *termEnv, termArgs) (string, error) { return helpText(), nil }},
	)
}

// hasSub reports whether a verb takes a sub-command or resource.
func hasSub(verb string) bool {
	switch verb {
	case "get", "describe", "rollout", "auth", "agent":
		return true
	}
	return false
}

// lookup finds the table row for parsed args.
func lookup(a termArgs) (termCmd, bool) {
	for _, c := range termCommands() {
		if c.Verb != a.verb {
			continue
		}
		if !hasSub(c.Verb) || c.Sub == a.sub {
			return c, true
		}
		for _, al := range c.Aliases {
			if al == a.sub {
				return c, true
			}
		}
	}
	return termCmd{}, false
}

// parseArgs splits a command line into its verb, sub, positionals and flags.
func parseArgs(line string) (termArgs, error) {
	a := termArgs{ns: "default", tail: 50}
	fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "kubectl "))
	var rest []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		name, val, hasVal := strings.Cut(f, "=")
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(fields) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return fields[i], nil
		}
		var err error
		switch name {
		case "-n", "--namespace":
			a.ns, err = next()
		case "-A", "--all-namespaces":
			a.allNs = true
		case "-c", "--container":
			a.container, err = next()
		case "--tail":
			var v string
			if v, err = next(); err == nil {
				a.tail, err = strconv.ParseInt(v, 10, 64)
			}
		case "-p", "--previous":
			a.previous = true
		case "--since":
			var v string
			if v, err = next(); err == nil {
				a.since, err = time.ParseDuration(v)
			}
		case "-o", "--output":
			a.output, err = next()
		case "-l", "--selector":
			a.selector, err = next()
		case "--field-selector":
			a.fieldSelector, err = next()
		case "--for":
			a.forObj, err = next()
		default:
			if strings.HasPrefix(f, "-") {
				return a, fmt.Errorf("unknown flag %s; type help", f)
			}
			rest = append(rest, f)
		}
		if err != nil {
			return a, fmt.Errorf("bad value for %s: %v", name, err)
		}
	}
	if len(rest) == 0 {
		return a, fmt.Errorf("empty command; type help")
	}
	a.verb, rest = rest[0], rest[1:]
	if hasSub(a.verb) && len(rest) > 0 {
		a.sub, rest = rest[0], rest[1:]
	}
	a.pos = rest
	if a.output != "" && a.output != "wide" {
		return a, fmt.Errorf("-o %s is not supported; only -o wide", a.output)
	}
	return a, nil
}

// refuse returns the reason a command is never run, or "".
func refuse(a termArgs) string {
	verbs := refusedVerbs()
	if why := verbs[a.verb]; why != "" {
		return why
	}
	if a.verb == "rollout" {
		if why := verbs["rollout "+a.sub]; why != "" {
			return why
		}
	}
	if (a.verb == "get" || a.verb == "describe") && (a.sub == "secret" || a.sub == "secrets") {
		return neverAdded()[2].Why
	}
	return ""
}

// executeKubectl runs one command line: parse, refuse, check the scope,
// run, redact, cap.
func executeKubectl(ctx context.Context, e *termEnv, line string) (string, error) {
	if words := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "kubectl ")); len(words) > 0 {
		// A refused command is refused whatever its flags say.
		sub := ""
		if len(words) > 1 {
			sub = words[1]
		}
		if why := refuse(termArgs{verb: words[0], sub: sub}); why != "" {
			return "", fmt.Errorf("`%s` is not available: %s", strings.TrimSpace(words[0]+" "+map[bool]string{true: sub}[hasSub(words[0])]), why)
		}
	}
	a, err := parseArgs(line)
	if err != nil {
		return "", err
	}
	if why := refuse(a); why != "" {
		return "", fmt.Errorf("`%s` is not available: %s", strings.TrimSpace(a.verb+" "+a.sub), why)
	}
	c, ok := lookup(a)
	if !ok {
		return "", fmt.Errorf("unsupported command: %s; type help", strings.TrimSpace(a.verb+" "+a.sub))
	}
	if !c.Cluster {
		if err := checkScope(e, a); err != nil {
			return "", err
		}
	}
	out, err := c.run(ctx, e, a)
	out = redact.String(out)
	if len(out) > maxTermOutput {
		out = out[:maxTermOutput] + "\n... output cut at 256 KiB"
	}
	return out, err
}

// checkScope applies the watch scope to namespaced reads (constraint 4);
// -A is expanded later to the watched namespaces only.
func checkScope(e *termEnv, a termArgs) error {
	if e.allowNS == nil {
		return fmt.Errorf("namespace %q is outside the watch scope", a.ns)
	}
	if a.allNs {
		return nil
	}
	if !e.allowNS(a.ns) {
		return fmt.Errorf("namespace %q is outside the watch scope", a.ns)
	}
	return nil
}

// namespaces is where a namespaced command reads: -n, or with -A every
// watched namespace (ISS-063: -A used to be refused outright).
func namespaces(ctx context.Context, e *termEnv, a termArgs) ([]string, error) {
	if !a.allNs {
		return []string{a.ns}, nil
	}
	list, err := e.kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		if e.allowNS(list.Items[i].Name) {
			out = append(out, list.Items[i].Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// helpText is `help`, made from the command table.
func helpText() string {
	var b strings.Builder
	b.WriteString("Read-only kubectl. You can run:\n")
	for _, c := range termCommands() {
		fmt.Fprintf(&b, "  %-70s %s\n", c.Usage, c.About)
	}
	b.WriteString("\nNot available, and why:\n")
	for _, r := range neverAdded() {
		fmt.Fprintf(&b, "  %s: %s\n", r.Commands, r.Why)
	}
	return b.String()
}

// handleKubectl runs one terminal command.
func (s *Server) handleKubectl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, kubectlResponse{Error: "POST required"})
		return
	}
	if s.kc == nil {
		writeJSON(w, kubectlResponse{Error: "no cluster connection"})
		return
	}
	var req kubectlRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, kubectlResponse{Error: "invalid request"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	out, err := executeKubectl(ctx, s.termEnv(), req.Command)
	resp := kubectlResponse{Output: out}
	if err != nil {
		resp.Error = redact.String(err.Error())
	}
	writeJSON(w, resp)
}

// handleKubectlHelp serves the command table for the terminal's panel.
func (s *Server) handleKubectlHelp(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"commands": termCommands(),
		"refused":  neverAdded(),
		"rules": []string{
			"Read-only: every change goes through the mutation gate, its guardrails and the audit trail.",
			"Namespaced reads stay inside the watch scope; -A means every watched namespace.",
			"Secrets are never read; ConfigMaps show key names, not values; every output is redacted.",
			"Each command needs only the permissions the agent already holds.",
		},
	})
}

func (s *Server) termEnv() *termEnv {
	return &termEnv{kc: s.kc, dyn: s.dyn, allowNS: s.allowNS, agent: s.agent, recorder: s.recorder, now: time.Now()}
}

package kube

import (
	"regexp"
	"strconv"
	"strings"
)

// schedCause is one reason the scheduler gave for rejecting nodes
// (PLAN-002 10.5 and 10.11).
type schedCause struct {
	Kind  string // taint, affinity, resources, volume, topology, other
	Nodes int    // how many nodes were rejected for this reason; 0 if not stated
	Text  string // the scheduler's own words for this reason
}

var nodeCountPrefix = regexp.MustCompile(`^(\d+) (?:node\(s\)|nodes?) `)

// schedKinds maps a phrase in the scheduler message to a cause. Order
// matters: the first match wins.
var schedKinds = []struct{ phrase, kind string }{
	{"taint", "taint"},
	{"topology spread", "topology"},
	{"volume node affinity", "volume"}, // before the plain "affinity" phrase
	{"affinity", "affinity"},
	{"node selector", "affinity"},
	{"Insufficient", "resources"},
	{"Too many pods", "resources"},
	{"PersistentVolumeClaim", "volume"},
	{"persistentvolumeclaim", "volume"},
}

// parseSchedulingFailure splits a FailedScheduling or PodScheduled message,
// for example
//
//	0/3 nodes are available: 1 node(s) had untolerated taint {k: v},
//	2 Insufficient cpu. preemption: 0/3 nodes are available: ...
//
// into one cause per reason. The preemption tail repeats the same reasons
// and is dropped.
func parseSchedulingFailure(msg string) []schedCause {
	body := msg
	if i := strings.Index(body, " preemption:"); i >= 0 {
		body = body[:i]
	}
	if i := strings.Index(body, "available: "); i >= 0 {
		body = body[i+len("available: "):]
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), ".")
	if body == "" {
		return nil
	}
	var out []schedCause
	for _, part := range splitTopLevel(body) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		c := schedCause{Kind: "other", Text: part}
		if m := nodeCountPrefix.FindStringSubmatch(part); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				c.Nodes = v
			}
			c.Text = strings.TrimSpace(part[len(m[0]):])
		} else if n, rest, ok := strings.Cut(part, " "); ok {
			// "2 Insufficient cpu" has a count but no "node(s)".
			if v, err := strconv.Atoi(n); err == nil {
				c.Nodes, c.Text = v, rest
			}
		}
		for _, k := range schedKinds {
			if strings.Contains(c.Text, k.phrase) {
				c.Kind = k.kind
				break
			}
		}
		out = append(out, c)
	}
	return out
}

// splitTopLevel splits on ", " outside braces, so a taint such as
// {a: b, c: d} stays in one piece.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// schedFix is the guidance for one kind of scheduling failure.
func schedFix(kind string) string {
	switch kind {
	case "taint":
		return "add a matching toleration to the pod, or remove the taint from nodes meant to run it"
	case "affinity":
		return "check nodeSelector and node or pod affinity against the labels nodes actually carry"
	case "resources":
		return "lower the pod's requests, free capacity, or add nodes (cluster autoscaler or node pool size)"
	case "volume":
		return "bind the PersistentVolumeClaim first; a zonal volume also needs a node in its zone"
	case "topology":
		return "the spread constraint cannot be met with the current nodes; relax maxSkew, use whenUnsatisfiable: ScheduleAnyway, or add nodes in the missing domain"
	default:
		return "read the scheduler message above; describe the pod for the full event"
	}
}

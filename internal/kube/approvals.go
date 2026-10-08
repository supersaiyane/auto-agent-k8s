package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// Approval queue (PLAN-002 phase 15, C1). An R3 fix is not applied when it
// is found: the exact change waits here until a listed approver accepts it
// through the signed Slack callback, someone rejects it, or it expires.
// Approving sends it through applyMutation once, so mode, fix scope,
// guardrails and the rate limiter are checked again at that moment. The
// queue lives in memory on the leader; a callback that reaches another
// replica, or arrives after a restart, finds no such approval (ISS-078).

const (
	approvalPending  = "pending"
	approvalApproved = "approved" // accepted; result holds what the gate did
	approvalRejected = "rejected"
	approvalExpired  = "expired"

	maxApprovals = 200 // decided items are dropped first when full
)

var (
	ErrApprovalsOff      = errors.New("approvals are off: set approvals.groups")
	ErrApprovalUnknown   = errors.New("no such approval on this replica")
	ErrApprovalDecided   = errors.New("approval already decided")
	ErrNotApprover       = errors.New("not a listed approver")
	errApprovalQueueFull = errors.New("approval queue is full")
)

// pendingApproval is one change waiting for a person.
type pendingApproval struct {
	id               string
	m                mutation
	summary          string
	created, expires time.Time
	state, by        string
	result           string // gate outcome once approved
}

// ApprovalView is what the dashboard and the API show for one approval.
type ApprovalView struct {
	ID        string    `json:"id"`
	Namespace string    `json:"namespace"`
	Workload  string    `json:"workload"`
	Reason    string    `json:"reason"`
	Action    string    `json:"action"`
	Change    string    `json:"change"`
	Summary   string    `json:"summary"`
	State     string    `json:"state"`
	By        string    `json:"by,omitempty"`
	Result    string    `json:"result,omitempty"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
}

// Approvals holds the queue. A nil *Approvals, or one with no approvers,
// is disabled: R3 fixes then stay suggestions.
type Approvals struct {
	mu        sync.Mutex
	ttl       time.Duration
	approvers map[string]bool
	items     map[string]*pendingApproval
}

// NewApprovals builds a queue whose items expire after ttl. Approvers are
// identities such as "slack:U123ABC".
func NewApprovals(ttl time.Duration, approvers []string) *Approvals {
	a := &Approvals{ttl: ttl, approvers: map[string]bool{}, items: map[string]*pendingApproval{}}
	for _, id := range approvers {
		if id != "" {
			a.approvers[id] = true
		}
	}
	return a
}

// Enabled reports whether anyone may approve.
func (a *Approvals) Enabled() bool { return a != nil && len(a.approvers) > 0 }

// IsApprover reports whether id may approve or reject from Slack.
func (a *Approvals) IsApprover(id string) bool { return a.Enabled() && a.approvers[id] }

func newApprovalID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("approval id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// expireLocked marks pending items past their expiry. Callers hold mu.
func (a *Approvals) expireLocked(now time.Time) {
	for _, p := range a.items {
		if p.state == approvalPending && !now.Before(p.expires) {
			p.state = approvalExpired
		}
	}
}

// pruneLocked drops the oldest decided items until there is room.
func (a *Approvals) pruneLocked() bool {
	if len(a.items) < maxApprovals {
		return true
	}
	decided := make([]*pendingApproval, 0, len(a.items))
	for _, p := range a.items {
		if p.state != approvalPending {
			decided = append(decided, p)
		}
	}
	sort.Slice(decided, func(i, j int) bool { return decided[i].created.Before(decided[j].created) })
	for _, p := range decided {
		if len(a.items) < maxApprovals {
			break
		}
		delete(a.items, p.id)
	}
	return len(a.items) < maxApprovals
}

// propose queues m, or returns the pending item already queued for the
// same change on the same workload.
func (a *Approvals) propose(m mutation, summary string, now time.Time) (*pendingApproval, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(now)
	for _, p := range a.items {
		if p.state == approvalPending && p.m.ActionType == m.ActionType && p.m.Namespace == m.Namespace && p.m.Workload == m.Workload && p.m.SuggestMsg == m.SuggestMsg {
			return p, false, nil
		}
	}
	if !a.pruneLocked() {
		return nil, false, errApprovalQueueFull
	}
	id, err := newApprovalID()
	if err != nil {
		return nil, false, err
	}
	p := &pendingApproval{id: id, m: m, summary: summary, created: now, expires: now.Add(a.ttl), state: approvalPending}
	a.items[id] = p
	return p, true, nil
}

// proposable reports whether an approval could ever apply m: the mode is
// fix and the namespace is inside the fix scope.
func proposable(pol *policy.Policy, m mutation) bool {
	return pol.Mode == policy.Fix && (m.Namespace == "" || pol.Fixable(m.Namespace))
}

// queueApproval puts a finding's proposed change in the queue. It returns
// the queued item (nil when nothing was queued), whether it is new, and
// the note that goes into the finding's message.
func queueApproval(deps *Deps, f finding) (*pendingApproval, bool, string) {
	a := deps.Approvals
	switch {
	case !a.Enabled():
		return nil, false, "approve to fix is off: no approvers are set (approvals.groups)"
	case !proposable(deps.Policy(), *f.Proposal):
		return nil, false, "approve to fix needs fix mode and this namespace inside the fix scope"
	}
	p, isNew, err := a.propose(*f.Proposal, f.Summary, deps.clock())
	if err != nil {
		klog.Warningf("approvals: cannot queue %s %s/%s: %v", f.Proposal.ActionType, f.Namespace, f.Workload, err)
		return nil, false, "approve to fix could not queue this change: " + err.Error()
	}
	return p, isNew, fmt.Sprintf("approval `%s` waits for an approver until %s: %s", p.id, p.expires.UTC().Format(time.RFC3339), f.Proposal.SuggestMsg)
}

// ApproveFix applies a queued change once, as user. The gate checks mode,
// scope, guardrails and the rate limiter again; a second approval of the
// same id is refused.
func ApproveFix(ctx context.Context, deps *Deps, id, user string) (string, error) {
	a := deps.Approvals
	if !a.Enabled() {
		return "", ErrApprovalsOff
	}
	a.mu.Lock()
	p := a.items[id]
	if p == nil {
		a.mu.Unlock()
		return "", ErrApprovalUnknown
	}
	m := p.m
	if !a.approvers[user] {
		a.mu.Unlock()
		klog.Warningf("approvals: %s is not an approver, refused %s", user, id)
		auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "blocked", "approval refused: "+user+" is not an approver")
		return "", ErrNotApprover
	}
	a.expireLocked(deps.clock())
	if p.state != approvalPending {
		state := p.state
		a.mu.Unlock()
		return "", fmt.Errorf("%w: %s", ErrApprovalDecided, state)
	}
	p.state, p.by = approvalApproved, user
	a.mu.Unlock()

	m.ApprovedBy = user
	out, msg := applyMutation(ctx, deps, m)
	a.mu.Lock()
	p.result = outcomeNames[out]
	a.mu.Unlock()
	return msg, nil
}

// RejectFix drops a pending change; by names who rejected it.
func RejectFix(deps *Deps, id, by string) error {
	a := deps.Approvals
	if !a.Enabled() {
		return ErrApprovalsOff
	}
	a.mu.Lock()
	p := a.items[id]
	if p == nil {
		a.mu.Unlock()
		return ErrApprovalUnknown
	}
	a.expireLocked(deps.clock())
	if p.state != approvalPending {
		state := p.state
		a.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrApprovalDecided, state)
	}
	p.state, p.by = approvalRejected, by
	m := p.m
	a.mu.Unlock()
	auditAction(deps, m.ActionType, m.Namespace, m.Workload, m.Pod, m.Reason, "rejected", "rejected by "+by)
	return nil
}

// ListApprovals returns every item, newest first.
func ListApprovals(deps *Deps) []ApprovalView {
	a := deps.Approvals
	if a == nil {
		return []ApprovalView{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireLocked(deps.clock())
	out := make([]ApprovalView, 0, len(a.items))
	for _, p := range a.items {
		out = append(out, ApprovalView{ID: p.id, Namespace: p.m.Namespace, Workload: p.m.Workload, Reason: p.m.Reason,
			Action: p.m.ActionType, Change: p.m.SuggestMsg, Summary: p.summary, State: p.state, By: p.by, Result: p.result,
			Created: p.created, Expires: p.expires})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

var outcomeNames = map[gateOutcome]string{
	gateSkipped: "skipped", gateSuggested: "suggested", gateSimulated: "simulated",
	gateBlocked: "blocked", gateFailed: "failed", gateApplied: "applied",
}

// blockPoster is the part of the Slack client that posts Block Kit.
type blockPoster interface {
	PostBlocks(blocks []map[string]interface{}) error
}

// postApprovalButtons sends the Approve and Reject buttons for a newly
// queued change, when the Slack client can post blocks. The button value
// is only the approval id; the change itself never leaves the agent.
func postApprovalButtons(deps *Deps, p *pendingApproval) {
	bp, ok := deps.Slack.(blockPoster)
	if !ok {
		return
	}
	text := fmt.Sprintf("*Approve to fix* `%s`: %s\n%s", p.id, p.m.SuggestMsg, p.summary)
	blocks := []map[string]interface{}{
		{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}},
		{"type": "actions", "elements": []map[string]interface{}{
			{"type": "button", "text": map[string]string{"type": "plain_text", "text": "Approve"}, "style": "primary",
				"action_id": "approve_fix", "value": p.id},
			{"type": "button", "text": map[string]string{"type": "plain_text", "text": "Reject"}, "style": "danger",
				"action_id": "reject_fix", "value": p.id},
		}},
	}
	if err := bp.PostBlocks(blocks); err != nil {
		klog.Warningf("approvals: posting buttons for %s: %v", p.id, err)
	}
}

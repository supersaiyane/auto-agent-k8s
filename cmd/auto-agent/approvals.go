package main

import (
	"context"
	"errors"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpapi"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
)

// approvalsAPI adapts the kube approval queue to the HTTP server, which
// does not import kube (PLAN-002 phase 15).
type approvalsAPI struct{ a *agent }

// newApprovalQueue builds the approval queue. On controllers it is stored
// in the agent's own ConfigMap, so pending approvals survive a restart and
// a change of leader (ISS-078); node agents never propose or approve.
func newApprovalQueue(a *agent, kc kubernetes.Interface) *kube.Approvals {
	q := kube.NewApprovals(a.conf.Approvals.TTL, a.conf.Approvals.Approvers)
	if a.rl.controller {
		q = q.StoreIn(kc, a.podNS)
	}
	return q
}

// approvalsFor serves the queue on controllers; node agents have none.
func approvalsFor(a *agent) httpapi.Approvals {
	if !a.rl.controller {
		return nil
	}
	return approvalsAPI{a}
}

func (x approvalsAPI) List() []httpapi.ApprovalItem {
	views := kube.ListApprovals(x.a.deps)
	out := make([]httpapi.ApprovalItem, 0, len(views))
	for _, v := range views {
		out = append(out, httpapi.ApprovalItem{ID: v.ID, Namespace: v.Namespace, Workload: v.Workload, Reason: v.Reason,
			Action: v.Action, Change: v.Change, Summary: v.Summary, State: v.State, By: v.By, Result: v.Result,
			Created: v.Created.UTC().Format(time.RFC3339), Expires: v.Expires.UTC().Format(time.RFC3339)})
	}
	return out
}

func (x approvalsAPI) Approve(ctx context.Context, id, user string) (string, error) {
	msg, err := kube.ApproveFix(ctx, x.a.deps, id, user)
	return msg, approvalErr(err)
}

func (x approvalsAPI) Reject(id, by string) error {
	return approvalErr(kube.RejectFix(x.a.deps, id, by))
}

func (x approvalsAPI) IsApprover(user string) bool { return x.a.deps.Approvals.IsApprover(user) }

// approvalErr maps queue errors to the ones the server turns into status
// codes. The message stays the queue's own: the server's sentinel says the
// same thing, and prefixing it repeated the reason (ISS-091).
func approvalErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kube.ErrApprovalUnknown):
		return mappedErr{err: err, kind: httpapi.ErrApprovalNotFound}
	case errors.Is(err, kube.ErrApprovalDecided):
		return mappedErr{err: err, kind: httpapi.ErrApprovalConflict}
	}
	return err
}

// mappedErr is err, also matching kind with errors.Is.
type mappedErr struct{ err, kind error }

func (m mappedErr) Error() string   { return m.err.Error() }
func (m mappedErr) Unwrap() []error { return []error{m.kind, m.err} }

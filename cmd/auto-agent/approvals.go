package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/supersaiyane/auto-agent-k8s/internal/httpapi"
	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
)

// approvalsAPI adapts the kube approval queue to the HTTP server, which
// does not import kube (PLAN-002 phase 15).
type approvalsAPI struct{ a *agent }

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
// codes, keeping the message.
func approvalErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, kube.ErrApprovalUnknown):
		return fmt.Errorf("%w: %v", httpapi.ErrApprovalNotFound, err)
	case errors.Is(err, kube.ErrApprovalDecided):
		return fmt.Errorf("%w: %v", httpapi.ErrApprovalConflict, err)
	}
	return err
}

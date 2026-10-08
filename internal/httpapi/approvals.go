package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"k8s.io/klog/v2"
)

// Approvals is the R3 approval queue as the server sees it (PLAN-002
// phase 15). The dashboard lists and rejects; only a listed approver,
// through the signed Slack callback, approves.
type Approvals interface {
	List() []ApprovalItem
	Approve(ctx context.Context, id, user string) (string, error)
	Reject(id, by string) error
	IsApprover(user string) bool
}

// ApprovalItem is one queued change.
type ApprovalItem struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Reason    string `json:"reason"`
	Action    string `json:"action"`
	Change    string `json:"change"`
	Summary   string `json:"summary"`
	State     string `json:"state"`
	By        string `json:"by,omitempty"`
	Result    string `json:"result,omitempty"`
	Created   string `json:"created"`
	Expires   string `json:"expires"`
}

// dashboardRejecter names the dashboard in the audit log; it shares one
// token, so it cannot name a person.
const dashboardRejecter = "dashboard"

// Errors the queue returns, matched here without importing it.
var (
	ErrApprovalNotFound = errors.New("approval not found")
	ErrApprovalConflict = errors.New("approval already decided")
)

// handleApprovals serves GET (the queue, watched namespaces only) and
// DELETE ?id= (reject). Approving is not offered here.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "approvals are served by the controller", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		items := []ApprovalItem{}
		for _, it := range s.approvals.List() {
			if s.allowNS == nil || it.Namespace == "" || s.allowNS(it.Namespace) {
				items = append(items, it)
			}
		}
		writeJSON(w, items)
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		if err := s.approvals.Reject(id, dashboardRejecter); err != nil {
			http.Error(w, err.Error(), approvalStatus(err))
			return
		}
		writeJSON(w, map[string]string{"id": id, "state": "rejected"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func approvalStatus(err error) int {
	switch {
	case errors.Is(err, ErrApprovalNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrApprovalConflict):
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}

// slackApprove and slackReject answer the Approve and Reject buttons. The
// user is "slack:<user id>", checked against the approver list.
func slackApprove(ctx context.Context, a Approvals, id, user string) string {
	if !a.IsApprover(user) {
		klog.Warningf("slack: %s pressed Approve on %s but is not an approver", user, id)
		return fmt.Sprintf("%s is not a listed approver; nothing was changed.", user)
	}
	msg, err := a.Approve(ctx, id, user)
	if err != nil {
		return fmt.Sprintf("Approval %s was not applied: %v", id, err)
	}
	return fmt.Sprintf("Approved by %s. %s", user, msg)
}

func slackReject(a Approvals, id, user string) string {
	if !a.IsApprover(user) {
		klog.Warningf("slack: %s pressed Reject on %s but is not an approver", user, id)
		return fmt.Sprintf("%s is not a listed approver; nothing was changed.", user)
	}
	if err := a.Reject(id, user); err != nil {
		return fmt.Sprintf("Approval %s was not rejected: %v", id, err)
	}
	return fmt.Sprintf("Rejected by %s; nothing was changed.", user)
}

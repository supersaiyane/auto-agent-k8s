package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeApprovals records what the server asked of the queue.
type fakeApprovals struct {
	items              []ApprovalItem
	approvers          map[string]bool
	approved, rejected []string
	rejectErr          error
}

func (f *fakeApprovals) List() []ApprovalItem { return f.items }
func (f *fakeApprovals) Approve(_ context.Context, id, user string) (string, error) {
	f.approved = append(f.approved, id+" by "+user)
	return "_Action_: raised maxReplicas.", nil
}
func (f *fakeApprovals) Reject(id, by string) error {
	if f.rejectErr != nil {
		return f.rejectErr
	}
	f.rejected = append(f.rejected, id+" by "+by)
	return nil
}
func (f *fakeApprovals) IsApprover(user string) bool { return f.approvers[user] }

func slackAction(actionID, value, user string) string {
	p := fmt.Sprintf(`{"type":"block_actions","user":{"id":%q,"username":"alice"},"actions":[{"action_id":%q,"value":%q}]}`, user, actionID, value)
	return "payload=" + url.QueryEscape(p)
}

func pressButton(h http.Handler, actionID, value, user string) string {
	body := slackAction(actionID, value, user)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	return postSlack(h, body, now, sign(testSigningSecret, now, body)).Body.String()
}

func TestSlackActions_ApproveAndRejectNeedAListedApprover(t *testing.T) {
	q := &fakeApprovals{approvers: map[string]bool{"slack:U1": true}}
	h := NewSlackActionHandler(testSigningSecret)
	h.approvals = q

	if got := pressButton(h, "approve_fix", "abc", "U9"); !strings.Contains(got, "not a listed approver") || len(q.approved) != 0 {
		t.Fatalf("an unlisted user approved: %s %v", got, q.approved)
	}
	if got := pressButton(h, "reject_fix", "abc", "U9"); !strings.Contains(got, "not a listed approver") || len(q.rejected) != 0 {
		t.Fatalf("an unlisted user rejected: %s %v", got, q.rejected)
	}
	if got := pressButton(h, "approve_fix", "abc", "U1"); !strings.Contains(got, "Approved by slack:U1") || len(q.approved) != 1 || q.approved[0] != "abc by slack:U1" {
		t.Fatalf("approve: %s %v", got, q.approved)
	}
	if got := pressButton(h, "reject_fix", "def", "U1"); !strings.Contains(got, "Rejected by slack:U1") || q.rejected[0] != "def by slack:U1" {
		t.Fatalf("reject: %s %v", got, q.rejected)
	}
	q.rejectErr = ErrApprovalConflict
	if got := pressButton(h, "reject_fix", "def", "U1"); !strings.Contains(got, "was not rejected") {
		t.Fatalf("a failed reject must say so: %s", got)
	}
}

func TestSlackActions_RejectWithoutQueueSaysNothingHappened(t *testing.T) {
	if got := pressButton(NewSlackActionHandler(testSigningSecret), "reject_fix", "abc", "U1"); !strings.Contains(got, "no action was taken") {
		t.Fatalf("got %s", got)
	}
}

func approvalsServer(q Approvals) *Server {
	return &Server{approvals: q, allowNS: func(ns string) bool { return ns == "default" }}
}

func callApprovals(s *Server, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handleApprovals(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestAPI_ApprovalsListFiltersAndRejects(t *testing.T) {
	q := &fakeApprovals{items: []ApprovalItem{{ID: "a", Namespace: "default"}, {ID: "b", Namespace: "secret-team"}}}
	s := approvalsServer(q)

	rec := callApprovals(s, http.MethodGet, "/api/approvals")
	var got []ApprovalItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("list must show watched namespaces only: %s %v", rec.Body.String(), err)
	}
	if rec := callApprovals(s, http.MethodDelete, "/api/approvals?id=a"); rec.Code != http.StatusOK || q.rejected[0] != "a by dashboard" {
		t.Fatalf("reject: %d %v", rec.Code, q.rejected)
	}
	cases := []struct {
		method, target string
		err            error
		want           int
	}{
		{http.MethodDelete, "/api/approvals", nil, http.StatusBadRequest},
		{http.MethodPost, "/api/approvals?id=a", nil, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/approvals?id=x", fmt.Errorf("%w: gone", ErrApprovalNotFound), http.StatusNotFound},
		{http.MethodDelete, "/api/approvals?id=x", fmt.Errorf("%w: expired", ErrApprovalConflict), http.StatusConflict},
	}
	for _, c := range cases {
		q.rejectErr = c.err
		if rec := callApprovals(s, c.method, c.target); rec.Code != c.want {
			t.Errorf("%s %s with %v: %d, want %d", c.method, c.target, c.err, rec.Code, c.want)
		}
	}
	if rec := callApprovals(&Server{}, http.MethodGet, "/api/approvals"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no queue: %d", rec.Code)
	}
}

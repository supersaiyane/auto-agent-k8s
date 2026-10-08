package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/supersaiyane/auto-agent-k8s/internal/kube"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// ScopeOptions serve /api/scope, the Settings tab (ADR-002). Both nil
// disables the route.
type ScopeOptions struct {
	// Policy returns the current policy snapshot.
	Policy func() *policy.Policy
	// Save stores the fix scope choice; names nil returns to the Helm list.
	// It refuses namespaces outside the ceiling and audits every attempt.
	Save func(ctx context.Context, names []string, from string) error
}

// scopeNamespace is one watched namespace as the Settings tab shows it.
type scopeNamespace struct {
	Name      string `json:"name"`
	InCeiling bool   `json:"inCeiling"` // fixing may be enabled from the dashboard
	Fixable   bool   `json:"fixable"`   // the agent may act here now
}

// scopeView is the GET /api/scope answer.
type scopeView struct {
	WatchAll    bool             `json:"watchAll"`
	FixAnywhere bool             `json:"fixAnywhere"`
	FixScope    []string         `json:"fixScope"`
	HelmFix     []string         `json:"helmFix"` // FIX_NAMESPACES inside the ceiling
	Choice      []string         `json:"choice"`  // the dashboard choice; null when none
	Namespaces  []scopeNamespace `json:"namespaces"`
}

// scopeRequest is the PUT /api/scope body. Every namespace being enabled
// must be typed again in Confirm, so a click alone never widens the scope.
type scopeRequest struct {
	FixNamespaces []string `json:"fixNamespaces"`
	Confirm       []string `json:"confirm"`
}

func (s *Server) handleScope(w http.ResponseWriter, r *http.Request) {
	if s.scope.Policy == nil || s.scope.Save == nil {
		http.Error(w, "scope settings are served by the controller", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getScope(w, r)
	case http.MethodPut:
		s.putScope(w, r)
	case http.MethodDelete:
		s.saveScope(w, r, nil)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getScope(w http.ResponseWriter, r *http.Request) {
	pol := s.scope.Policy()
	view := scopeView{WatchAll: pol.WatchAll, FixAnywhere: pol.FixAnywhere, FixScope: pol.FixScope(),
		HelmFix: pol.WithFixOverride(nil).FixScope(), Namespaces: []scopeNamespace{}}
	if pol.FixOverride != nil {
		view.Choice = pol.FixScope()
	}
	names, all := pol.WatchList()
	if all {
		list, err := s.kc.CoreV1().Namespaces().List(r.Context(), metav1.ListOptions{})
		if err != nil {
			http.Error(w, "cannot list namespaces: "+err.Error(), http.StatusBadGateway)
			return
		}
		for i := range list.Items {
			names = append(names, list.Items[i].Name)
		}
		sort.Strings(names)
	}
	for _, ns := range names {
		if pol.Watched(ns) {
			view.Namespaces = append(view.Namespaces, scopeNamespace{Name: ns, InCeiling: pol.InCeiling(ns), Fixable: pol.Fixable(ns)})
		}
	}
	writeJSON(w, view)
}

func (s *Server) putScope(w http.ResponseWriter, r *http.Request) {
	var req scopeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil || req.FixNamespaces == nil {
		http.Error(w, `body must be {"fixNamespaces": [...], "confirm": [...]}`, http.StatusBadRequest)
		return
	}
	names := policy.ScopeChoice(map[string]string{policy.ScopeKey: strings.Join(req.FixNamespaces, ",")})
	if missing := unconfirmed(s.scope.Policy(), names, req.Confirm); len(missing) > 0 {
		http.Error(w, "type each namespace you enable to confirm it: "+strings.Join(missing, ", "), http.StatusBadRequest)
		return
	}
	s.saveScope(w, r, names)
}

// unconfirmed lists the namespaces names enables that confirm does not repeat.
func unconfirmed(pol *policy.Policy, names, confirm []string) []string {
	typed := policy.NamespaceSet(confirm...)
	var missing []string
	for _, n := range names {
		if _, ok := typed[n]; !ok && !pol.Fixable(n) {
			missing = append(missing, n)
		}
	}
	return missing
}

func (s *Server) saveScope(w http.ResponseWriter, r *http.Request, names []string) {
	err := s.scope.Save(r.Context(), names, requestOrigin(r))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, kube.ErrOutsideCeiling):
		http.Error(w, fmt.Sprintf("%v: change agent.fixCeiling in Helm to allow it", err), http.StatusBadRequest)
	default:
		http.Error(w, "saving the fix scope failed: "+err.Error(), http.StatusBadGateway)
	}
}

// requestOrigin names where a change came from, for the audit log: the
// address a standby controller forwarded, else the direct peer.
func requestOrigin(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" && r.Header.Get(proxiedHeader) != "" {
		return "dashboard via " + strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "dashboard via " + host
}

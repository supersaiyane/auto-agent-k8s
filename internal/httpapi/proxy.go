package httpapi

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"

	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// proxiedHeader marks a request a standby already forwarded, so two
// controllers that disagree about the leader cannot bounce it forever.
const proxiedHeader = "X-Auto-Agent-Proxied"

// toLeader sends API and ingest requests to the leader when this
// controller is the standby (ADR-001): the leader holds the one event log,
// so every request through the Service sees the same view. The leader
// checks the token itself; the standby forwards the request unchanged.
func (s *Server) toLeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		forward := p == "/api" || strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/internal/")
		if !forward || s.leader == nil || r.Header.Get(proxiedHeader) != "" {
			next.ServeHTTP(w, r)
			return
		}
		target, err := s.leader()
		if err != nil {
			obs.HandlerErrorsTotal.WithLabelValues("proxy", "no_leader").Inc()
			w.Header().Set("Retry-After", "2")
			http.Error(w, "no leader yet: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if target == "" { // this pod leads
			next.ServeHTTP(w, r)
			return
		}
		u, err := url.Parse(target)
		if err != nil {
			obs.HandlerErrorsTotal.WithLabelValues("proxy", "bad_target").Inc()
			http.Error(w, "bad leader address", http.StatusBadGateway)
			return
		}
		rp := httputil.NewSingleHostReverseProxy(u)
		if s.http != nil && s.http.Transport != nil {
			rp.Transport = s.http.Transport
		}
		rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			obs.HandlerErrorsTotal.WithLabelValues("proxy", "leader_unreachable").Inc()
			klog.V(2).Infof("proxy: leader %s unreachable: %v", target, err)
			w.Header().Set("Retry-After", "2")
			http.Error(w, "leader unreachable", http.StatusBadGateway)
		}
		r.Header.Set(proxiedHeader, "1")
		rp.ServeHTTP(w, r)
	})
}

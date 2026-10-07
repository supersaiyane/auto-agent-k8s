package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// ISS-051, ISS-060: every response, page and API alike, carries the
// browser protections; the policy allows no inline script or style.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s := newTestServer(t, "tok")
	for _, path := range []string{"/", "/healthz", "/api/status", "/metrics"} {
		w := httptest.NewRecorder()
		s.srv.Handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		h := w.Header()
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") ||
			!strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
}

// Package httpxtest gives tests an *http.Client that sends every request,
// whatever its host (api.github.com, events.pagerduty.com, ...), to a local
// httptest server, and records what was sent (PLAN-002 9.1, 9.2).
package httpxtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Request is one recorded request.
type Request struct {
	Method string
	Path   string // path and query, as the client built them
	Host   string // the host the client meant to reach
	Body   string
}

// Server is an httptest server with a request log.
type Server struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []Request
}

// New starts a server that records each request and then calls h.
func New(h http.HandlerFunc) *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, Request{Method: r.Method, Path: r.URL.RequestURI(), Host: r.Header.Get("X-Original-Host"), Body: string(b)})
		s.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		h(w, r)
	}))
	return s
}

// Requests returns a copy of the recorded requests.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

// Client returns a client that redirects every request to the server and
// gives up after timeout.
func (s *Server) Client(timeout time.Duration) *http.Client {
	target, _ := url.Parse(s.URL)
	return &http.Client{Timeout: timeout, Transport: redirect{target: target}}
}

type redirect struct{ target *url.URL }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.Header.Set("X-Original-Host", req.URL.Host)
	out.URL.Scheme, out.URL.Host, out.Host = r.target.Scheme, r.target.Host, r.target.Host
	return http.DefaultTransport.RoundTrip(out)
}

// JSON writes body with status.
func JSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// Slow answers after d, longer than a test client's timeout.
func Slow(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
		}
		JSON(w, 200, `{}`)
	}
}

package httpapi

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

// Server provides health, readiness, and metrics endpoints.
type Server struct {
	srv   *http.Server
	ready int32
}

func NewServer(addr string) *Server {
	s := &Server{}
	mux := http.NewServeMux()

	// Liveness: always OK if process is running
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Readiness: OK only after all dependencies are initialized
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&s.ready) == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	mux.Handle("/metrics", promhttp.Handler())

	s.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return s
}

// Start begins serving. Call in a goroutine.
func (s *Server) Start() {
	klog.Infof("httpapi: listening on %s", s.srv.Addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.Errorf("httpapi: server error: %v", err)
	}
}

// SetReady marks the server as ready to receive traffic.
func (s *Server) SetReady() {
	atomic.StoreInt32(&s.ready, 1)
	klog.Infof("httpapi: marked ready")
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	klog.Infof("httpapi: shutting down")
	return s.srv.Shutdown(ctx)
}

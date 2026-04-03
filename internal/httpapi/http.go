package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/events"
)

//go:embed ui/*
var uiFS embed.FS

// Server provides health, readiness, metrics, API, and UI endpoints.
type Server struct {
	srv      *http.Server
	ready    int32
	recorder *events.Recorder
	meta     *AgentMeta
}

// AgentMeta holds static info exposed via the status API.
type AgentMeta struct {
	Version  string `json:"version"`
	Mode     string `json:"mode"`
	NodeName string `json:"nodeName"`
	PodName  string `json:"podName"`
	IsLeaderFn func() bool
}

func NewServer(addr string, recorder *events.Recorder, meta *AgentMeta) *Server {
	s := &Server{recorder: recorder, meta: meta}
	mux := http.NewServeMux()

	// Health probes
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&s.ready) == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
		}
	})

	// Prometheus metrics
	mux.Handle("/metrics", promhttp.Handler())

	// API endpoints
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/stats", s.handleStats)

	// Embedded UI
	uiSub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		klog.Fatalf("httpapi: failed to sub embed FS: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(uiSub)))

	s.srv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return s
}

func (s *Server) Start() {
	klog.Infof("httpapi: listening on %s", s.srv.Addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.Errorf("httpapi: server error: %v", err)
	}
}

func (s *Server) SetLeaderFunc(fn func() bool) {
	s.meta.IsLeaderFn = fn
}

func (s *Server) SetReady() {
	atomic.StoreInt32(&s.ready, 1)
	klog.Infof("httpapi: marked ready")
}

func (s *Server) Shutdown(ctx context.Context) error {
	klog.Infof("httpapi: shutting down")
	return s.srv.Shutdown(ctx)
}

// --- API Handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	isLeader := false
	if s.meta.IsLeaderFn != nil {
		isLeader = s.meta.IsLeaderFn()
	}
	status := map[string]interface{}{
		"version":  s.meta.Version,
		"mode":     s.meta.Mode,
		"nodeName": s.meta.NodeName,
		"podName":  s.meta.PodName,
		"isLeader": isLeader,
		"ready":    atomic.LoadInt32(&s.ready) == 1,
		"uptime":   time.Since(startTime).String(),
		"startedAt": startTime.UTC().Format(time.RFC3339),
		"eventCount": s.recorder.Count(),
	}
	writeJSON(w, status)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	n := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
			if n > 500 {
				n = 500
			}
		}
	}

	typeFilter := r.URL.Query().Get("type")
	allEvents := s.recorder.Recent(n * 2) // fetch more, then filter

	if typeFilter != "" {
		filtered := make([]events.Event, 0)
		for _, e := range allEvents {
			if string(e.Type) == typeFilter {
				filtered = append(filtered, e)
				if len(filtered) >= n {
					break
				}
			}
		}
		writeJSON(w, filtered)
		return
	}

	if len(allEvents) > n {
		allEvents = allEvents[:n]
	}
	writeJSON(w, allEvents)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.recorder.Stats()
	result := map[string]interface{}{
		"total":     s.recorder.Count(),
		"byType":    stats,
	}
	writeJSON(w, result)
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(data)
}

var startTime = time.Now()

func init() {
	_ = os.Getenv("") // ensure init runs
}

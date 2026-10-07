package httpapi

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	srv      *http.Server
	ready    int32
	recorder *events.Recorder
	// internalToken authenticates forwarded events (ADR-001).
	internalToken string
	// leader resolves where a standby controller proxies to (ADR-001).
	leader func() (string, error)
	// ingest stores forwarded events.
	ingest  events.Sink
	meta    *AgentMeta
	kc      kubernetes.Interface
	token   string            // DASHBOARD_TOKEN; empty disables /api/ (ISS-005)
	allowNS func(string) bool // namespace allowlist for kubectl reads
	cost    CostConfig        // Cost tab pricing (PLAN-002 9.3)
	ext     ExtendedDeps      // extended endpoints (PLAN-002 9.4)
	http    *http.Client      // outbound calls (Kubecost, OpenCost)
	started time.Time
}

type AgentMeta struct {
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	NodeName   string `json:"nodeName"`
	PodName    string `json:"podName"`
	IsLeaderFn func() bool
}

// Options carries the server's secrets (PLAN-002 8.3: no env reads here).
type Options struct {
	DashboardToken     string       // bearer token for /api/; empty disables it (503)
	SlackSigningSecret string       // verifies Slack callbacks; empty rejects them (503)
	Cost               config.Cost  // Cost tab pricing
	Extended           ExtendedDeps // optional trackers for the extended endpoints
	HTTPClient         *http.Client // outbound calls; nil means a 10s-timeout client
	// Leader returns the leader's base URL when this controller is the
	// standby, "" when it leads, or an error before a leader is known.
	// nil means never proxy (ADR-001).
	Leader func() (string, error)
	// Ingest receives forwarded events; nil means the recorder. The
	// controller passes a Tee so ingested events are copied too (ISS-059).
	Ingest events.Sink
	// HealthOnly serves only the probes and /metrics, for node agents.
	HealthOnly bool
	// InternalToken authenticates node agents forwarding events to the
	// controller (ADR-001); empty disables the ingest endpoint.
	InternalToken  string
	AllowNamespace func(string) bool
	IsLeader       func() bool
}

func NewServer(addr string, recorder *events.Recorder, meta *AgentMeta, kc kubernetes.Interface, opts Options) *Server {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	s := &Server{recorder: recorder, meta: meta, kc: kc, token: opts.DashboardToken,
		cost: newCostConfig(opts.Cost), ext: opts.Extended, http: hc, started: time.Now(),
		allowNS: opts.AllowNamespace, internalToken: opts.InternalToken, leader: opts.Leader}
	s.ingest = opts.Ingest
	if s.ingest == nil && recorder != nil {
		s.ingest = recorder
	}
	if opts.IsLeader != nil {
		meta.IsLeaderFn = opts.IsLeader
	}
	if s.token == "" && !opts.HealthOnly {
		klog.Warningf("httpapi: DASHBOARD_TOKEN not set, /api/ is disabled")
	}
	mux := http.NewServeMux()

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

	mux.Handle("/metrics", promhttp.Handler())
	if opts.HealthOnly {
		s.srv = &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second,
			WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		return s
	}

	// API endpoints
	for _, rt := range apiRouteTable {
		h := rt.handler
		mux.HandleFunc(rt.path, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	}

	// Events forwarded by node agents; outside /api/, with its own token.
	mux.HandleFunc(events.IngestPath, s.handleIngest)

	// Slack interactive actions callback
	slackHandler := NewSlackActionHandler(opts.SlackSigningSecret)
	RegisterSlackActions(mux, slackHandler)

	// Embedded UI
	uiSub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		klog.Fatalf("httpapi: failed to sub embed FS: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(uiSub)))

	s.srv = &http.Server{
		Addr:         addr,
		Handler:      securityHeaders(s.toLeader(s.authorize(mux))),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return s
}

// contentSecurityPolicy allows only this origin's own scripts, styles and
// API calls: no inline script or style, no framing (ISS-051, ISS-060).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// securityHeaders sets the browser protections on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Start() {
	klog.Infof("httpapi: listening on %s", s.srv.Addr)
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		klog.Errorf("httpapi: server error: %v", err)
	}
}

func (s *Server) SetLeaderFunc(fn func() bool) { s.meta.IsLeaderFn = fn }

// SetNamespaceFilter sets the allowlist the kubectl endpoint enforces. Until
// it is set, namespaced kubectl reads are denied.
func (s *Server) SetNamespaceFilter(fn func(string) bool) { s.allowNS = fn }

// nsAllowed applies the namespace allowlist to dashboard reads (CLAUDE.md
// constraint 4, ISS-028). Until a filter is set, nothing is allowed.
func (s *Server) nsAllowed(ns string) bool { return s.allowNS != nil && s.allowNS(ns) }

// authorize requires the dashboard bearer token on every /api/ path except
// the Slack callback, which is verified by Slack signature (ISS-005, ISS-006).
// Probes, /metrics and the static UI files stay open.
func (s *Server) authorize(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if (p != "/api" && !strings.HasPrefix(p, "/api/")) || p == slackActionsPath {
			next.ServeHTTP(w, r)
			return
		}
		if s.token == "" {
			http.Error(w, "dashboard API disabled: set DASHBOARD_TOKEN", http.StatusServiceUnavailable)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="auto-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) SetReady() {
	atomic.StoreInt32(&s.ready, 1)
	klog.Infof("httpapi: marked ready")
}

func (s *Server) Shutdown(ctx context.Context) error {
	klog.Infof("httpapi: shutting down")
	return s.srv.Shutdown(ctx)
}

// --- Existing API Handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	isLeader := false
	if s.meta.IsLeaderFn != nil {
		isLeader = s.meta.IsLeaderFn()
	}
	writeJSON(w, map[string]interface{}{
		"version":    s.meta.Version,
		"mode":       s.meta.Mode,
		"nodeName":   s.meta.NodeName,
		"podName":    s.meta.PodName,
		"isLeader":   isLeader,
		"ready":      atomic.LoadInt32(&s.ready) == 1,
		"uptime":     time.Since(s.started).String(),
		"startedAt":  s.started.UTC().Format(time.RFC3339),
		"eventCount": s.recorder.Count(),
	})
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
	allEvents := s.recorder.Recent(n * 2)
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
	writeJSON(w, map[string]interface{}{
		"total":  s.recorder.Count(),
		"byType": s.recorder.Stats(),
	})
}

// --- Cluster Overview API ---

type nsOverview struct {
	Name        string     `json:"name"`
	Phase       string     `json:"phase"`
	Pods        podSummary `json:"pods"`
	Deployments int        `json:"deployments"`
	Services    int        `json:"services"`
	Jobs        int        `json:"jobs"`
}

type podSummary struct {
	Total     int `json:"total"`
	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Succeeded int `json:"succeeded"`
	CrashLoop int `json:"crashLoop"`
	NotReady  int `json:"notReady"`
}

// handleCluster returns overview of all namespaces.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, []nsOverview{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	nsList, err := s.kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]nsOverview, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		if !s.nsAllowed(ns.Name) {
			continue
		}
		ov := nsOverview{
			Name:  ns.Name,
			Phase: string(ns.Status.Phase),
		}

		pods, err := s.kc.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			obs.CountAPIError(err, "pods", ns.Name)
		}
		if pods != nil {
			ov.Pods = summarizePods(pods.Items)
		}

		deploys, err := s.kc.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			obs.CountAPIError(err, "deployments", ns.Name)
		}
		if deploys != nil {
			ov.Deployments = len(deploys.Items)
		}

		svcs, err := s.kc.CoreV1().Services(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			obs.CountAPIError(err, "services", ns.Name)
		}
		if svcs != nil {
			ov.Services = len(svcs.Items)
		}

		jobs, err := s.kc.BatchV1().Jobs(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			obs.CountAPIError(err, "jobs", ns.Name)
		}
		if jobs != nil {
			ov.Jobs = len(jobs.Items)
		}

		result = append(result, ov)
	}
	writeJSON(w, result)
}

type podDetail struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace"`
	Phase      string            `json:"phase"`
	Node       string            `json:"node"`
	IP         string            `json:"ip"`
	Ready      string            `json:"ready"`
	Status     string            `json:"status"`
	Restarts   int32             `json:"restarts"`
	Age        string            `json:"age"`
	Containers []containerDetail `json:"containers"`
}

type containerDetail struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    string `json:"state"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
}

type deployDetail struct {
	Name      string `json:"name"`
	Ready     string `json:"ready"`
	UpToDate  int32  `json:"upToDate"`
	Available int32  `json:"available"`
	Age       string `json:"age"`
}

type svcDetail struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	ClusterIP string `json:"clusterIP"`
	Ports     string `json:"ports"`
	Age       string `json:"age"`
}

type nsDetail struct {
	Pods        []podDetail    `json:"pods"`
	Deployments []deployDetail `json:"deployments"`
	Services    []svcDetail    `json:"services"`
}

// handleNamespace returns detailed resources for a namespace: /api/namespace/{name}
func (s *Server) handleNamespace(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, nsDetail{})
		return
	}
	// Extract namespace from path: /api/namespace/default
	ns := r.URL.Path[len("/api/namespace/"):]
	if ns == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}
	if !s.nsAllowed(ns) {
		http.Error(w, "namespace is not in the namespace allowlist", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	detail := nsDetail{}

	// Pods
	pods, err := s.kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		obs.CountAPIError(err, "pods", ns)
	}
	if pods != nil {
		for _, p := range pods.Items {
			pd := podDetail{
				Name:      p.Name,
				Namespace: p.Namespace,
				Phase:     string(p.Status.Phase),
				Node:      p.Spec.NodeName,
				IP:        p.Status.PodIP,
				Status:    podStatus(&p),
				Age:       age(p.CreationTimestamp.Time),
			}
			readyCount := 0
			totalCount := len(p.Spec.Containers)
			for _, cs := range p.Status.ContainerStatuses {
				cd := containerDetail{
					Name:     cs.Name,
					Image:    cs.Image,
					Ready:    cs.Ready,
					Restarts: cs.RestartCount,
				}
				if cs.State.Running != nil {
					cd.State = "Running"
				} else if cs.State.Waiting != nil {
					cd.State = cs.State.Waiting.Reason
				} else if cs.State.Terminated != nil {
					cd.State = cs.State.Terminated.Reason
				}
				if cs.Ready {
					readyCount++
				}
				pd.Restarts += cs.RestartCount
				pd.Containers = append(pd.Containers, cd)
			}
			pd.Ready = fmt.Sprintf("%d/%d", readyCount, totalCount)
			detail.Pods = append(detail.Pods, pd)
		}
	}

	// Deployments
	deploys, err := s.kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		obs.CountAPIError(err, "deployments", ns)
	}
	if deploys != nil {
		for _, d := range deploys.Items {
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			detail.Deployments = append(detail.Deployments, deployDetail{
				Name:      d.Name,
				Ready:     fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, desired),
				UpToDate:  d.Status.UpdatedReplicas,
				Available: d.Status.AvailableReplicas,
				Age:       age(d.CreationTimestamp.Time),
			})
		}
	}

	// Services
	svcs, err := s.kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		obs.CountAPIError(err, "services", ns)
	}
	if svcs != nil {
		for _, svc := range svcs.Items {
			ports := ""
			for i, p := range svc.Spec.Ports {
				if i > 0 {
					ports += ", "
				}
				ports += fmt.Sprintf("%d/%s", p.Port, p.Protocol)
				if p.NodePort > 0 {
					ports += fmt.Sprintf(":%d", p.NodePort)
				}
			}
			detail.Services = append(detail.Services, svcDetail{
				Name:      svc.Name,
				Type:      string(svc.Spec.Type),
				ClusterIP: svc.Spec.ClusterIP,
				Ports:     ports,
				Age:       age(svc.CreationTimestamp.Time),
			})
		}
	}

	writeJSON(w, detail)
}

type nodeInfo struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	Roles          string `json:"roles"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	CPU            string `json:"cpu"`
	Memory         string `json:"memory"`
	Pods           int    `json:"pods"`
	MemoryPressure bool   `json:"memoryPressure"`
	DiskPressure   bool   `json:"diskPressure"`
	Unschedulable  bool   `json:"unschedulable"`
	Age            string `json:"age"`
}

// handleNodes returns node status and health.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, []nodeInfo{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	nodes, err := s.kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]nodeInfo, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		ni := nodeInfo{
			Name:          n.Name,
			Status:        nodeStatus(&n),
			Version:       n.Status.NodeInfo.KubeletVersion,
			OS:            n.Status.NodeInfo.OSImage,
			Arch:          n.Status.NodeInfo.Architecture,
			CPU:           n.Status.Capacity.Cpu().String(),
			Memory:        formatMemory(n.Status.Capacity.Memory().Value()),
			Unschedulable: n.Spec.Unschedulable,
			Age:           age(n.CreationTimestamp.Time),
		}
		// Roles
		for k := range n.Labels {
			if k == "node-role.kubernetes.io/control-plane" {
				ni.Roles = "control-plane"
			} else if k == "node-role.kubernetes.io/worker" {
				if ni.Roles != "" {
					ni.Roles += ","
				}
				ni.Roles += "worker"
			}
		}
		if ni.Roles == "" {
			ni.Roles = "worker"
		}
		// Conditions
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
				ni.MemoryPressure = true
			}
			if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue {
				ni.DiskPressure = true
			}
		}
		// Pod count on this node
		pods, err := s.kc.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + n.Name,
		})
		if err != nil {
			obs.CountAPIError(err, "pods", "")
		}
		if pods != nil {
			ni.Pods = len(pods.Items)
		}
		result = append(result, ni)
	}
	writeJSON(w, result)
}

// --- Helpers ---

type k8sEvent struct {
	Timestamp string `json:"timestamp"`
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
	Count     int32  `json:"count"`
	Source    string `json:"source"`
	Age       string `json:"age"`
}

// handleK8sEvents returns real Kubernetes events from the cluster.
func (s *Server) handleK8sEvents(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, []k8sEvent{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	ns := r.URL.Query().Get("namespace")
	var eventList *corev1.EventList
	var err error
	if ns != "" && !s.nsAllowed(ns) {
		http.Error(w, "namespace is not in the namespace allowlist", http.StatusForbidden)
		return
	}
	if ns != "" {
		eventList, err = s.kc.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	} else {
		eventList, err = s.kc.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Only allowlisted namespaces (ISS-028).
	items := make([]corev1.Event, 0, len(eventList.Items))
	for _, e := range eventList.Items {
		if s.nsAllowed(e.Namespace) {
			items = append(items, e)
		}
	}
	// Sort by last timestamp descending, take most recent 200
	// Simple sort: reverse order (API returns chronological)
	result := make([]k8sEvent, 0, 200)
	start := 0
	if len(items) > 200 {
		start = len(items) - 200
	}
	for i := len(items) - 1; i >= start; i-- {
		e := items[i]
		ts := e.LastTimestamp.Time
		if ts.IsZero() {
			ts = e.CreationTimestamp.Time
		}
		result = append(result, k8sEvent{
			Timestamp: ts.UTC().Format(time.RFC3339),
			Namespace: e.Namespace,
			Kind:      e.InvolvedObject.Kind,
			Name:      e.InvolvedObject.Name,
			Type:      e.Type,
			Reason:    e.Reason,
			Message:   e.Message,
			Count:     e.Count,
			Source:    e.Source.Component,
			Age:       age(ts),
		})
	}
	writeJSON(w, result)
}

func summarizePods(pods []corev1.Pod) podSummary {
	var s podSummary
	s.Total = len(pods)
	for _, p := range pods {
		switch p.Status.Phase {
		case corev1.PodRunning:
			s.Running++
		case corev1.PodPending:
			s.Pending++
		case corev1.PodFailed:
			s.Failed++
		case corev1.PodSucceeded:
			s.Succeeded++
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
				s.CrashLoop++
			}
			if cs.State.Running != nil && !cs.Ready {
				s.NotReady++
			}
		}
	}
	return s
}

func podStatus(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
			return cs.State.Terminated.Reason
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return "Init:" + cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason == "Error" {
			return "Init:Error"
		}
	}
	return string(p.Status.Phase)
}

func nodeStatus(n *corev1.Node) string {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				return "Ready"
			}
			return "NotReady"
		}
	}
	return "Unknown"
}

func age(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func formatMemory(bytes int64) string {
	gi := float64(bytes) / (1024 * 1024 * 1024)
	if gi >= 1 {
		return fmt.Sprintf("%.1fGi", gi)
	}
	mi := float64(bytes) / (1024 * 1024)
	return fmt.Sprintf("%.0fMi", mi)
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(data)
}

// apiRouteTable is every dashboard API route. Registration and the auth test
// both read it, so a route added here is covered by the test automatically.
var apiRouteTable = []struct {
	path    string
	handler func(*Server, http.ResponseWriter, *http.Request)
}{
	{"/api/status", (*Server).handleStatus},
	{"/api/events", (*Server).handleEvents},
	{"/api/stats", (*Server).handleStats},
	{"/api/cluster", (*Server).handleCluster},
	{"/api/namespace/", (*Server).handleNamespace},
	{"/api/nodes", (*Server).handleNodes},
	{"/api/k8s-events", (*Server).handleK8sEvents},
	{"/api/kubectl", (*Server).handleKubectl},
	{"/api/compliance", (*Server).handleCompliance},
	{"/api/baselines", (*Server).handleBaselines},
	{"/api/deploys", (*Server).handleDeploys},
	{"/api/dry-run", (*Server).handleDryRun},
	{"/api/cost", (*Server).handleCost},
	{"/api/resources", (*Server).handleResources},
	{"/api/resources/", (*Server).handleResourcesNs},
	{"/api/fixes", (*Server).handleFixes},
}

// apiRoutes returns every /api/ path the server serves, Slack included.
func apiRoutes() []string {
	paths := []string{slackActionsPath}
	for _, rt := range apiRouteTable {
		paths = append(paths, rt.path)
	}
	return paths
}

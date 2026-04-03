package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/events"
)

//go:embed ui/*
var uiFS embed.FS

type Server struct {
	srv      *http.Server
	ready    int32
	recorder *events.Recorder
	meta     *AgentMeta
	kc       kubernetes.Interface
}

type AgentMeta struct {
	Version    string `json:"version"`
	Mode       string `json:"mode"`
	NodeName   string `json:"nodeName"`
	PodName    string `json:"podName"`
	IsLeaderFn func() bool
}

func NewServer(addr string, recorder *events.Recorder, meta *AgentMeta, kc kubernetes.Interface) *Server {
	s := &Server{recorder: recorder, meta: meta, kc: kc}
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

	// API endpoints
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/cluster", s.handleCluster)
	mux.HandleFunc("/api/namespace/", s.handleNamespace)
	mux.HandleFunc("/api/nodes", s.handleNodes)

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

func (s *Server) SetLeaderFunc(fn func() bool) { s.meta.IsLeaderFn = fn }

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
		"uptime":     time.Since(startTime).String(),
		"startedAt":  startTime.UTC().Format(time.RFC3339),
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
	Name        string       `json:"name"`
	Phase       string       `json:"phase"`
	Pods        podSummary   `json:"pods"`
	Deployments int          `json:"deployments"`
	Services    int          `json:"services"`
	Jobs        int          `json:"jobs"`
}

type podSummary struct {
	Total      int `json:"total"`
	Running    int `json:"running"`
	Pending    int `json:"pending"`
	Failed     int `json:"failed"`
	Succeeded  int `json:"succeeded"`
	CrashLoop  int `json:"crashLoop"`
	NotReady   int `json:"notReady"`
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
		ov := nsOverview{
			Name:  ns.Name,
			Phase: string(ns.Status.Phase),
		}

		pods, _ := s.kc.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
		if pods != nil {
			ov.Pods = summarizePods(pods.Items)
		}

		deploys, _ := s.kc.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
		if deploys != nil {
			ov.Deployments = len(deploys.Items)
		}

		svcs, _ := s.kc.CoreV1().Services(ns.Name).List(ctx, metav1.ListOptions{})
		if svcs != nil {
			ov.Services = len(svcs.Items)
		}

		jobs, _ := s.kc.BatchV1().Jobs(ns.Name).List(ctx, metav1.ListOptions{})
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
	Name       string `json:"name"`
	Type       string `json:"type"`
	ClusterIP  string `json:"clusterIP"`
	Ports      string `json:"ports"`
	Age        string `json:"age"`
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

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	detail := nsDetail{}

	// Pods
	pods, _ := s.kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
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
	deploys, _ := s.kc.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
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
	svcs, _ := s.kc.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
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
	Name            string `json:"name"`
	Status          string `json:"status"`
	Roles           string `json:"roles"`
	Version         string `json:"version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	CPU             string `json:"cpu"`
	Memory          string `json:"memory"`
	Pods            int    `json:"pods"`
	MemoryPressure  bool   `json:"memoryPressure"`
	DiskPressure    bool   `json:"diskPressure"`
	Unschedulable   bool   `json:"unschedulable"`
	Age             string `json:"age"`
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
		pods, _ := s.kc.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + n.Name,
		})
		if pods != nil {
			ni.Pods = len(pods.Items)
		}
		result = append(result, ni)
	}
	writeJSON(w, result)
}

// --- Helpers ---

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

var startTime = time.Now()

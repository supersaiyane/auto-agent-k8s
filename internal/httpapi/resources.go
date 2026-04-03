package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type nsResourceOverview struct {
	Name          string  `json:"name"`
	Pods          int     `json:"pods"`
	CPURequests   float64 `json:"cpuRequests"`
	CPULimits     float64 `json:"cpuLimits"`
	MemRequestsMi float64 `json:"memRequestsMi"`
	MemLimitsMi   float64 `json:"memLimitsMi"`
	Monthly       float64 `json:"monthly"`
	Overuse       int     `json:"overuse"`   // pods with no limits or limit >> request
	Underuse      int     `json:"underuse"`  // pods with very low requests
	NoLimits      int     `json:"noLimits"`  // pods with no resource limits at all
	Healthy       int     `json:"healthy"`
}

type podResourceDetail struct {
	Name          string                `json:"name"`
	Namespace     string                `json:"namespace"`
	Node          string                `json:"node"`
	Status        string                `json:"status"`
	Age           string                `json:"age"`
	Restarts      int32                 `json:"restarts"`
	Efficiency    string                `json:"efficiency"` // "overuse", "underuse", "right-sized", "no-limits"
	EffColor      string                `json:"effColor"`   // red, yellow, green, muted
	CPURequest    string                `json:"cpuRequest"`
	CPULimit      string                `json:"cpuLimit"`
	MemRequest    string                `json:"memRequest"`
	MemLimit      string                `json:"memLimit"`
	LimitReqRatio float64              `json:"limitReqRatio"` // limit/request ratio
	Monthly       float64              `json:"monthly"`
	Containers    []containerResource   `json:"containers"`
	Advice        string                `json:"advice"`
}

type containerResource struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	CPUReq     string `json:"cpuReq"`
	CPULim     string `json:"cpuLim"`
	MemReq     string `json:"memReq"`
	MemLim     string `json:"memLim"`
	Efficiency string `json:"efficiency"`
}

type nsResourceDetail struct {
	Namespace string               `json:"namespace"`
	Summary   nsResourceOverview   `json:"summary"`
	Pods      []podResourceDetail  `json:"pods"`
}

// handleResources returns all namespaces with resource overview.
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, []nsResourceOverview{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	nsList, err := s.kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]nsResourceOverview, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		ov := nsResourceOverview{Name: ns.Name}
		pods, _ := s.kc.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
		if pods == nil {
			result = append(result, ov)
			continue
		}
		ov.Pods = len(pods.Items)
		for _, p := range pods.Items {
			cpuReq, cpuLim, memReq, memLim := podFullResources(&p)
			ov.CPURequests += cpuReq
			ov.CPULimits += cpuLim
			ov.MemRequestsMi += memReq
			ov.MemLimitsMi += memLim

			eff := classifyPod(cpuReq, cpuLim, memReq, memLim)
			switch eff {
			case "overuse":
				ov.Overuse++
			case "underuse":
				ov.Underuse++
			case "no-limits":
				ov.NoLimits++
			default:
				ov.Healthy++
			}
		}
		ov.Monthly = round2(calcCost(ov.CPURequests, ov.MemRequestsMi))
		ov.CPURequests = round3(ov.CPURequests)
		ov.CPULimits = round3(ov.CPULimits)
		ov.MemRequestsMi = round1(ov.MemRequestsMi)
		ov.MemLimitsMi = round1(ov.MemLimitsMi)
		result = append(result, ov)
	}
	writeJSON(w, result)
}

// handleResourcesNs returns detailed pod resource usage for a namespace.
func (s *Server) handleResourcesNs(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, nsResourceDetail{})
		return
	}
	ns := r.URL.Path[len("/api/resources/"):]
	if ns == "" {
		http.Error(w, "namespace required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pods, err := s.kc.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	detail := nsResourceDetail{Namespace: ns}
	summary := nsResourceOverview{Name: ns}

	for _, p := range pods.Items {
		cpuReq, cpuLim, memReq, memLim := podFullResources(&p)
		eff := classifyPod(cpuReq, cpuLim, memReq, memLim)
		effColor := effToColor(eff)
		advice := effAdvice(eff, cpuReq, cpuLim, memReq, memLim)

		var ratio float64
		if cpuReq > 0 {
			ratio = cpuLim / cpuReq
		}

		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}

		pd := podResourceDetail{
			Name:          p.Name,
			Namespace:     ns,
			Node:          p.Spec.NodeName,
			Status:        podStatus(&p),
			Age:           age(p.CreationTimestamp.Time),
			Restarts:      restarts,
			Efficiency:    eff,
			EffColor:      effColor,
			CPURequest:    fmtCPU(cpuReq),
			CPULimit:      fmtCPU(cpuLim),
			MemRequest:    fmtMem(memReq),
			MemLimit:      fmtMem(memLim),
			LimitReqRatio: round2(ratio),
			Monthly:       round2(calcCost(cpuReq, memReq)),
			Advice:        advice,
		}

		for _, c := range p.Spec.Containers {
			cr := containerResource{Name: c.Name, Image: c.Image}
			if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
				cr.CPUReq = req.String()
			} else {
				cr.CPUReq = "-"
			}
			if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
				cr.CPULim = lim.String()
			} else {
				cr.CPULim = "-"
			}
			if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
				cr.MemReq = req.String()
			} else {
				cr.MemReq = "-"
			}
			if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				cr.MemLim = lim.String()
			} else {
				cr.MemLim = "-"
			}
			cr.Efficiency = classifyContainer(cr.CPUReq, cr.CPULim, cr.MemReq, cr.MemLim)
			pd.Containers = append(pd.Containers, cr)
		}

		detail.Pods = append(detail.Pods, pd)

		// Summary
		summary.CPURequests += cpuReq
		summary.CPULimits += cpuLim
		summary.MemRequestsMi += memReq
		summary.MemLimitsMi += memLim
		switch eff {
		case "overuse":
			summary.Overuse++
		case "underuse":
			summary.Underuse++
		case "no-limits":
			summary.NoLimits++
		default:
			summary.Healthy++
		}
	}
	summary.Pods = len(pods.Items)
	summary.Monthly = round2(calcCost(summary.CPURequests, summary.MemRequestsMi))
	detail.Summary = summary
	writeJSON(w, detail)
}

// classifyPod determines if a pod is overuse, underuse, right-sized, or has no limits.
func classifyPod(cpuReq, cpuLim, memReq, memLim float64) string {
	// No limits at all — dangerous
	if cpuLim == 0 && memLim == 0 {
		return "no-limits"
	}
	// No requests — likely overusing shared resources
	if cpuReq == 0 && memReq == 0 {
		return "overuse"
	}
	// Limit >> request (more than 5x) — overprovisioned, wasting reservation
	if cpuReq > 0 && cpuLim/cpuReq > 5 {
		return "overuse"
	}
	if memReq > 0 && memLim/memReq > 5 {
		return "overuse"
	}
	// Very small requests (< 10m CPU or < 16Mi memory) on a container that has limits
	if cpuReq > 0 && cpuReq < 0.01 && cpuLim > 0.1 {
		return "underuse"
	}
	if memReq > 0 && memReq < 16 && memLim > 128 {
		return "underuse"
	}
	return "right-sized"
}

func classifyContainer(cpuReq, cpuLim, memReq, memLim string) string {
	if cpuLim == "-" && memLim == "-" {
		return "no-limits"
	}
	if cpuReq == "-" && memReq == "-" {
		return "no-requests"
	}
	return "ok"
}

func effToColor(eff string) string {
	switch eff {
	case "overuse":
		return "red"
	case "underuse":
		return "yellow"
	case "no-limits":
		return "red"
	default:
		return "green"
	}
}

func effAdvice(eff string, cpuReq, cpuLim, memReq, memLim float64) string {
	switch eff {
	case "no-limits":
		return "Set CPU and memory limits to prevent node exhaustion"
	case "overuse":
		if cpuReq == 0 {
			return "Set CPU/memory requests — pod may be starving other workloads"
		}
		if cpuReq > 0 && cpuLim/cpuReq > 5 {
			return fmt.Sprintf("Limit/request ratio is %.0fx — reduce limits closer to actual usage", cpuLim/cpuReq)
		}
		return "Resource limits too high relative to requests"
	case "underuse":
		return "Very low resource requests — pod may get throttled under load. Increase requests."
	default:
		return ""
	}
}

func podFullResources(p *corev1.Pod) (cpuReq, cpuLim, memReqMi, memLimMi float64) {
	for _, c := range p.Spec.Containers {
		if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuReq += req.AsApproximateFloat64()
		}
		if lim, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			cpuLim += lim.AsApproximateFloat64()
		}
		if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			memReqMi += req.AsApproximateFloat64() / (1024 * 1024)
		}
		if lim, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			memLimMi += lim.AsApproximateFloat64() / (1024 * 1024)
		}
	}
	return
}

func fmtCPU(cores float64) string {
	if cores == 0 {
		return "-"
	}
	if cores < 1 {
		return fmt.Sprintf("%dm", int(cores*1000))
	}
	return fmt.Sprintf("%.1f", cores)
}

func fmtMem(mi float64) string {
	if mi == 0 {
		return "-"
	}
	if mi >= 1024 {
		return fmt.Sprintf("%.1fGi", mi/1024)
	}
	return fmt.Sprintf("%.0fMi", mi)
}

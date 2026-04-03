package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Configurable pricing (defaults to AWS on-demand rough averages)
var (
	cpuPricePerHour    = 0.05   // $/vCPU/hour
	memPricePerGiBHour = 0.005  // $/GiB/hour
	hoursPerMonth      = 730.0
)

type clusterCost struct {
	TotalMonthly    float64          `json:"totalMonthly"`
	NodeCost        float64          `json:"nodeCost"`
	WorkloadCost    float64          `json:"workloadCost"`
	WastedCost      float64          `json:"wastedCost"`
	Nodes           []nodeCost       `json:"nodes"`
	Namespaces      []namespaceCost  `json:"namespaces"`
	TopWorkloads    []workloadCost   `json:"topWorkloads"`
	Summary         costSummary      `json:"summary"`
}

type nodeCost struct {
	Name       string  `json:"name"`
	CPUCores   float64 `json:"cpuCores"`
	MemoryGiB  float64 `json:"memoryGiB"`
	Pods       int     `json:"pods"`
	Monthly    float64 `json:"monthly"`
	CPUUsedPct float64 `json:"cpuUsedPct"`
	MemUsedPct float64 `json:"memUsedPct"`
}

type namespaceCost struct {
	Name       string  `json:"name"`
	Pods       int     `json:"pods"`
	CPUReq     float64 `json:"cpuRequests"`
	MemReqMiB  float64 `json:"memRequestsMiB"`
	Monthly    float64 `json:"monthly"`
}

type workloadCost struct {
	Namespace  string  `json:"namespace"`
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	Replicas   int32   `json:"replicas"`
	CPUReq     float64 `json:"cpuRequests"`
	MemReqMiB  float64 `json:"memRequestsMiB"`
	Monthly    float64 `json:"monthly"`
}

type costSummary struct {
	TotalCPUCores    float64 `json:"totalCpuCores"`
	TotalMemoryGiB   float64 `json:"totalMemoryGiB"`
	AllocatedCPU     float64 `json:"allocatedCpu"`
	AllocatedMemGiB  float64 `json:"allocatedMemGiB"`
	CPUUtilization   float64 `json:"cpuUtilization"`
	MemUtilization   float64 `json:"memUtilization"`
	TotalPods        int     `json:"totalPods"`
	TotalNodes       int     `json:"totalNodes"`
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, clusterCost{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	result := clusterCost{}

	// --- Nodes ---
	nodes, err := s.kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var totalCPU, totalMem, allocCPU, allocMem float64
	for _, n := range nodes.Items {
		cpuCores := n.Status.Capacity.Cpu().AsApproximateFloat64()
		memBytes := n.Status.Capacity.Memory().AsApproximateFloat64()
		memGiB := memBytes / (1024 * 1024 * 1024)

		// Count pods and their resource requests on this node
		pods, _ := s.kc.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + n.Name,
		})
		podCount := 0
		var nodeCPUReq, nodeMemReq float64
		if pods != nil {
			podCount = len(pods.Items)
			for _, p := range pods.Items {
				cpu, mem := podResourceRequests(&p)
				nodeCPUReq += cpu
				nodeMemReq += mem
			}
		}

		monthly := (cpuCores * cpuPricePerHour + memGiB * memPricePerGiBHour) * hoursPerMonth
		cpuPct := 0.0
		if cpuCores > 0 {
			cpuPct = (nodeCPUReq / cpuCores) * 100
		}
		memPct := 0.0
		if memGiB > 0 {
			memPct = (nodeMemReq / 1024 / memGiB) * 100 // nodeMemReq is in MiB
		}

		result.Nodes = append(result.Nodes, nodeCost{
			Name: n.Name, CPUCores: cpuCores, MemoryGiB: round2(memGiB),
			Pods: podCount, Monthly: round2(monthly),
			CPUUsedPct: round1(cpuPct), MemUsedPct: round1(memPct),
		})
		result.NodeCost += monthly
		totalCPU += cpuCores
		totalMem += memGiB
		allocCPU += nodeCPUReq
		allocMem += nodeMemReq / 1024 // MiB to GiB
	}

	// --- Namespaces + Workloads ---
	nsList, _ := s.kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	var allWorkloads []workloadCost
	if nsList != nil {
		for _, ns := range nsList.Items {
			nsCost := namespaceCost{Name: ns.Name}
			pods, _ := s.kc.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
			if pods != nil {
				nsCost.Pods = len(pods.Items)
				for _, p := range pods.Items {
					cpu, mem := podResourceRequests(&p)
					nsCost.CPUReq += cpu
					nsCost.MemReqMiB += mem
				}
			}
			nsCost.Monthly = round2(calcMonthly(nsCost.CPUReq, nsCost.MemReqMiB))
			result.Namespaces = append(result.Namespaces, nsCost)
			result.WorkloadCost += nsCost.Monthly

			// Deployments
			deploys, _ := s.kc.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
			if deploys != nil {
				for _, d := range deploys.Items {
					replicas := int32(1)
					if d.Spec.Replicas != nil {
						replicas = *d.Spec.Replicas
					}
					cpu, mem := templateResourceRequests(&d.Spec.Template.Spec)
					monthly := calcMonthly(cpu*float64(replicas), mem*float64(replicas))
					allWorkloads = append(allWorkloads, workloadCost{
						Namespace: ns.Name, Name: d.Name, Kind: "Deployment",
						Replicas: replicas, CPUReq: round3(cpu), MemReqMiB: round1(mem),
						Monthly: round2(monthly),
					})
				}
			}

			// StatefulSets
			stss, _ := s.kc.AppsV1().StatefulSets(ns.Name).List(ctx, metav1.ListOptions{})
			if stss != nil {
				for _, sts := range stss.Items {
					replicas := int32(1)
					if sts.Spec.Replicas != nil {
						replicas = *sts.Spec.Replicas
					}
					cpu, mem := templateResourceRequests(&sts.Spec.Template.Spec)
					monthly := calcMonthly(cpu*float64(replicas), mem*float64(replicas))
					allWorkloads = append(allWorkloads, workloadCost{
						Namespace: ns.Name, Name: sts.Name, Kind: "StatefulSet",
						Replicas: replicas, CPUReq: round3(cpu), MemReqMiB: round1(mem),
						Monthly: round2(monthly),
					})
				}
			}
		}
	}

	// Sort workloads by cost descending, take top 20
	sortWorkloads(allWorkloads)
	if len(allWorkloads) > 20 {
		result.TopWorkloads = allWorkloads[:20]
	} else {
		result.TopWorkloads = allWorkloads
	}

	// Wasted = node capacity cost - workload cost
	result.TotalMonthly = round2(result.NodeCost)
	result.WastedCost = round2(result.NodeCost - result.WorkloadCost)
	if result.WastedCost < 0 {
		result.WastedCost = 0 // overcommitted
	}
	result.WorkloadCost = round2(result.WorkloadCost)

	result.Summary = costSummary{
		TotalCPUCores:   round1(totalCPU),
		TotalMemoryGiB:  round1(totalMem),
		AllocatedCPU:    round2(allocCPU),
		AllocatedMemGiB: round2(allocMem),
		TotalPods:       countAllPods(result.Namespaces),
		TotalNodes:      len(result.Nodes),
	}
	if totalCPU > 0 {
		result.Summary.CPUUtilization = round1(allocCPU / totalCPU * 100)
	}
	if totalMem > 0 {
		result.Summary.MemUtilization = round1(allocMem / totalMem * 100)
	}

	writeJSON(w, result)
}

func podResourceRequests(p *corev1.Pod) (cpuCores float64, memMiB float64) {
	for _, c := range p.Spec.Containers {
		if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuCores += req.AsApproximateFloat64()
		}
		if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			memMiB += req.AsApproximateFloat64() / (1024 * 1024)
		}
	}
	return
}

func templateResourceRequests(spec *corev1.PodSpec) (cpuCores float64, memMiB float64) {
	for _, c := range spec.Containers {
		if req, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuCores += req.AsApproximateFloat64()
		}
		if req, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			memMiB += req.AsApproximateFloat64() / (1024 * 1024)
		}
	}
	return
}

func calcMonthly(cpuCores, memMiB float64) float64 {
	return (cpuCores*cpuPricePerHour + (memMiB/1024)*memPricePerGiBHour) * hoursPerMonth
}

func sortWorkloads(w []workloadCost) {
	// Simple insertion sort (small list)
	for i := 1; i < len(w); i++ {
		for j := i; j > 0 && w[j].Monthly > w[j-1].Monthly; j-- {
			w[j], w[j-1] = w[j-1], w[j]
		}
	}
}

func countAllPods(nss []namespaceCost) int {
	total := 0
	for _, ns := range nss {
		total += ns.Pods
	}
	return total
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }

// Ensure fmt is available for future use
var _ = fmt.Sprintf

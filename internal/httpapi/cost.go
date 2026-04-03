package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// CostConfig holds pricing configuration — loaded from env vars.
type CostConfig struct {
	CPUPerHour    float64            `json:"cpuPerHour"`    // $/vCPU/hour
	MemPerGiBHour float64           `json:"memPerGiBHour"` // $/GiB/hour
	HoursPerMonth float64           `json:"hoursPerMonth"`
	Source        string            `json:"source"`        // "manual", "kubecost", "opencost", "aws", "gcp", "azure"
	Currency      string            `json:"currency"`
	InstancePrices map[string]float64 `json:"instancePrices,omitempty"` // instance-type -> $/hour
	KubecostURL   string            `json:"-"`
	OpenCostURL   string            `json:"-"`
}

var costCfg CostConfig

func init() {
	costCfg = CostConfig{
		CPUPerHour:    envFloat("COST_CPU_PER_HOUR", 0.05),
		MemPerGiBHour: envFloat("COST_MEM_PER_GIB_HOUR", 0.005),
		HoursPerMonth: 730.0,
		Currency:      envStr("COST_CURRENCY", "USD"),
		KubecostURL:   envStr("KUBECOST_URL", ""),   // e.g. http://kubecost-cost-analyzer.kubecost:9090
		OpenCostURL:   envStr("OPENCOST_URL", ""),    // e.g. http://opencost.opencost:9003
		InstancePrices: make(map[string]float64),
	}

	// Determine source
	if costCfg.KubecostURL != "" {
		costCfg.Source = "kubecost"
	} else if costCfg.OpenCostURL != "" {
		costCfg.Source = "opencost"
	} else if os.Getenv("COST_CPU_PER_HOUR") != "" {
		costCfg.Source = "manual"
	} else {
		costCfg.Source = "default"
	}

	// Load instance-type prices from env: COST_INSTANCE_PRICES="t3.medium=0.0416,m5.xlarge=0.192"
	if prices := envStr("COST_INSTANCE_PRICES", ""); prices != "" {
		for _, pair := range strings.Split(prices, ",") {
			parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(parts) == 2 {
				if v, err := strconv.ParseFloat(parts[1], 64); err == nil {
					costCfg.InstancePrices[parts[0]] = v
				}
			}
		}
		if len(costCfg.InstancePrices) > 0 {
			costCfg.Source = "instance-type"
			klog.Infof("cost: loaded %d instance-type prices", len(costCfg.InstancePrices))
		}
	}

	klog.Infof("cost: source=%s, cpu=$%.4f/hr, mem=$%.4f/GiB/hr, currency=%s",
		costCfg.Source, costCfg.CPUPerHour, costCfg.MemPerGiBHour, costCfg.Currency)
}

type clusterCost struct {
	TotalMonthly  float64         `json:"totalMonthly"`
	NodeCost      float64         `json:"nodeCost"`
	WorkloadCost  float64         `json:"workloadCost"`
	WastedCost    float64         `json:"wastedCost"`
	Nodes         []nodeCost      `json:"nodes"`
	Namespaces    []namespaceCost `json:"namespaces"`
	TopWorkloads  []workloadCost  `json:"topWorkloads"`
	Summary       costSummary     `json:"summary"`
	Config        CostConfig      `json:"config"`
}

type nodeCost struct {
	Name         string  `json:"name"`
	InstanceType string  `json:"instanceType"`
	CPUCores     float64 `json:"cpuCores"`
	MemoryGiB    float64 `json:"memoryGiB"`
	Pods         int     `json:"pods"`
	Monthly      float64 `json:"monthly"`
	HourlyRate   float64 `json:"hourlyRate"`
	CPUUsedPct   float64 `json:"cpuUsedPct"`
	MemUsedPct   float64 `json:"memUsedPct"`
	PriceSource  string  `json:"priceSource"` // "instance-type", "computed", "kubecost"
}

type namespaceCost struct {
	Name      string  `json:"name"`
	Pods      int     `json:"pods"`
	CPUReq    float64 `json:"cpuRequests"`
	MemReqMiB float64 `json:"memRequestsMiB"`
	Monthly   float64 `json:"monthly"`
}

type workloadCost struct {
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Replicas  int32   `json:"replicas"`
	CPUReq    float64 `json:"cpuRequests"`
	MemReqMiB float64 `json:"memRequestsMiB"`
	Monthly   float64 `json:"monthly"`
}

type costSummary struct {
	TotalCPUCores   float64 `json:"totalCpuCores"`
	TotalMemoryGiB  float64 `json:"totalMemoryGiB"`
	AllocatedCPU    float64 `json:"allocatedCpu"`
	AllocatedMemGiB float64 `json:"allocatedMemGiB"`
	CPUUtilization  float64 `json:"cpuUtilization"`
	MemUtilization  float64 `json:"memUtilization"`
	TotalPods       int     `json:"totalPods"`
	TotalNodes      int     `json:"totalNodes"`
}

func (s *Server) handleCost(w http.ResponseWriter, r *http.Request) {
	if s.kc == nil {
		writeJSON(w, clusterCost{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Try Kubecost/OpenCost first if configured
	if costCfg.KubecostURL != "" {
		if data, err := fetchKubecostData(ctx); err == nil {
			writeJSON(w, data)
			return
		}
		klog.V(3).Infof("cost: kubecost fetch failed, falling back to computed")
	}
	if costCfg.OpenCostURL != "" {
		if data, err := fetchOpenCostData(ctx); err == nil {
			writeJSON(w, data)
			return
		}
		klog.V(3).Infof("cost: opencost fetch failed, falling back to computed")
	}

	result := clusterCost{Config: costCfg}

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

		// Determine hourly rate
		instanceType := n.Labels["node.kubernetes.io/instance-type"]
		if instanceType == "" {
			instanceType = n.Labels["beta.kubernetes.io/instance-type"]
		}
		hourly, priceSource := nodeHourlyRate(instanceType, cpuCores, memGiB)

		// Count pods and resource usage
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

		monthly := hourly * costCfg.HoursPerMonth
		cpuPct := safePct(nodeCPUReq, cpuCores)
		memPct := safePct(nodeMemReq/1024, memGiB)

		result.Nodes = append(result.Nodes, nodeCost{
			Name: n.Name, InstanceType: instanceType,
			CPUCores: cpuCores, MemoryGiB: round2(memGiB),
			Pods: podCount, Monthly: round2(monthly), HourlyRate: round4(hourly),
			CPUUsedPct: round1(cpuPct), MemUsedPct: round1(memPct),
			PriceSource: priceSource,
		})
		result.NodeCost += monthly
		totalCPU += cpuCores
		totalMem += memGiB
		allocCPU += nodeCPUReq
		allocMem += nodeMemReq / 1024
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
			nsCost.Monthly = round2(calcCost(nsCost.CPUReq, nsCost.MemReqMiB))
			result.Namespaces = append(result.Namespaces, nsCost)
			result.WorkloadCost += nsCost.Monthly

			deploys, _ := s.kc.AppsV1().Deployments(ns.Name).List(ctx, metav1.ListOptions{})
			if deploys != nil {
				for _, d := range deploys.Items {
					replicas := int32(1)
					if d.Spec.Replicas != nil {
						replicas = *d.Spec.Replicas
					}
					cpu, mem := templateResourceRequests(&d.Spec.Template.Spec)
					monthly := calcCost(cpu*float64(replicas), mem*float64(replicas))
					allWorkloads = append(allWorkloads, workloadCost{
						Namespace: ns.Name, Name: d.Name, Kind: "Deployment",
						Replicas: replicas, CPUReq: round3(cpu), MemReqMiB: round1(mem),
						Monthly: round2(monthly),
					})
				}
			}
			stss, _ := s.kc.AppsV1().StatefulSets(ns.Name).List(ctx, metav1.ListOptions{})
			if stss != nil {
				for _, sts := range stss.Items {
					replicas := int32(1)
					if sts.Spec.Replicas != nil {
						replicas = *sts.Spec.Replicas
					}
					cpu, mem := templateResourceRequests(&sts.Spec.Template.Spec)
					monthly := calcCost(cpu*float64(replicas), mem*float64(replicas))
					allWorkloads = append(allWorkloads, workloadCost{
						Namespace: ns.Name, Name: sts.Name, Kind: "StatefulSet",
						Replicas: replicas, CPUReq: round3(cpu), MemReqMiB: round1(mem),
						Monthly: round2(monthly),
					})
				}
			}
		}
	}

	sortWorkloads(allWorkloads)
	if len(allWorkloads) > 20 {
		result.TopWorkloads = allWorkloads[:20]
	} else {
		result.TopWorkloads = allWorkloads
	}

	result.TotalMonthly = round2(result.NodeCost)
	result.WastedCost = round2(result.NodeCost - result.WorkloadCost)
	if result.WastedCost < 0 {
		result.WastedCost = 0
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

// nodeHourlyRate returns the hourly rate for a node.
// Priority: instance-type lookup > computed from CPU+mem.
func nodeHourlyRate(instanceType string, cpuCores, memGiB float64) (float64, string) {
	// Check instance-type price table
	if instanceType != "" {
		if price, ok := costCfg.InstancePrices[instanceType]; ok {
			return price, "instance-type"
		}
		// Well-known AWS instance types
		if price, ok := awsPricing[instanceType]; ok {
			return price, "aws-known"
		}
	}
	// Compute from CPU + memory
	return cpuCores*costCfg.CPUPerHour + memGiB*costCfg.MemPerGiBHour, "computed"
}

// awsPricing — common AWS on-demand prices (us-east-1, USD/hour, approximate)
var awsPricing = map[string]float64{
	// General purpose
	"t3.micro": 0.0104, "t3.small": 0.0208, "t3.medium": 0.0416, "t3.large": 0.0832, "t3.xlarge": 0.1664,
	"t3a.micro": 0.0094, "t3a.small": 0.0188, "t3a.medium": 0.0376, "t3a.large": 0.0752,
	"m5.large": 0.096, "m5.xlarge": 0.192, "m5.2xlarge": 0.384, "m5.4xlarge": 0.768,
	"m5a.large": 0.086, "m5a.xlarge": 0.172, "m5a.2xlarge": 0.344,
	"m6i.large": 0.096, "m6i.xlarge": 0.192, "m6i.2xlarge": 0.384,
	"m6g.medium": 0.0385, "m6g.large": 0.077, "m6g.xlarge": 0.154, "m6g.2xlarge": 0.308,
	"m7g.medium": 0.0408, "m7g.large": 0.0816, "m7g.xlarge": 0.1632,
	// Compute optimized
	"c5.large": 0.085, "c5.xlarge": 0.17, "c5.2xlarge": 0.34, "c5.4xlarge": 0.68,
	"c6g.large": 0.068, "c6g.xlarge": 0.136, "c6g.2xlarge": 0.272,
	"c6i.large": 0.085, "c6i.xlarge": 0.17, "c6i.2xlarge": 0.34,
	"c7g.large": 0.0725, "c7g.xlarge": 0.145,
	// Memory optimized
	"r5.large": 0.126, "r5.xlarge": 0.252, "r5.2xlarge": 0.504,
	"r6g.large": 0.1008, "r6g.xlarge": 0.2016,
	"r6i.large": 0.126, "r6i.xlarge": 0.252,
	// GCP equivalents (approximate)
	"n2-standard-2": 0.0971, "n2-standard-4": 0.1942, "n2-standard-8": 0.3885,
	"e2-medium": 0.0335, "e2-standard-2": 0.067, "e2-standard-4": 0.134,
	"n2d-standard-2": 0.0845, "n2d-standard-4": 0.169,
	// Azure equivalents (approximate)
	"Standard_B2s": 0.0416, "Standard_B2ms": 0.0832,
	"Standard_D2s_v3": 0.096, "Standard_D4s_v3": 0.192, "Standard_D8s_v3": 0.384,
	"Standard_D2as_v4": 0.086, "Standard_D4as_v4": 0.172,
}

func calcCost(cpuCores, memMiB float64) float64 {
	return (cpuCores*costCfg.CPUPerHour + (memMiB/1024)*costCfg.MemPerGiBHour) * costCfg.HoursPerMonth
}

// --- Kubecost integration ---

func fetchKubecostData(ctx context.Context) (*clusterCost, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	// Kubecost allocation API: /model/allocation?window=1d&aggregate=namespace
	url := costCfg.KubecostURL + "/model/allocation?window=30d&aggregate=namespace&accumulate=true"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubecost: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("kubecost: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	var kcResp struct {
		Code int `json:"code"`
		Data []map[string]struct {
			Name       string `json:"name"`
			CPUCost    float64 `json:"cpuCost"`
			MemCost    float64 `json:"ramCost"`
			PVCost     float64 `json:"pvCost"`
			TotalCost  float64 `json:"totalCost"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &kcResp); err != nil {
		return nil, fmt.Errorf("kubecost: parse: %w", err)
	}

	result := &clusterCost{
		Config: costCfg,
	}
	result.Config.Source = "kubecost"

	if len(kcResp.Data) > 0 {
		for nsName, alloc := range kcResp.Data[0] {
			result.Namespaces = append(result.Namespaces, namespaceCost{
				Name:    nsName,
				Monthly: round2(alloc.TotalCost),
			})
			result.WorkloadCost += alloc.TotalCost
			result.TotalMonthly += alloc.TotalCost
		}
	}
	return result, nil
}

// --- OpenCost integration ---

func fetchOpenCostData(ctx context.Context) (*clusterCost, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	// OpenCost allocation API
	url := costCfg.OpenCostURL + "/allocation/compute?window=30d&aggregate=namespace&accumulate=true"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencost: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("opencost: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))

	var ocResp struct {
		Code int `json:"code"`
		Data []map[string]struct {
			Name      string  `json:"name"`
			TotalCost float64 `json:"totalCost"`
			CPUCost   float64 `json:"cpuCost"`
			RAMCost   float64 `json:"ramCost"`
			PVCost    float64 `json:"pvCost"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ocResp); err != nil {
		return nil, fmt.Errorf("opencost: parse: %w", err)
	}

	result := &clusterCost{Config: costCfg}
	result.Config.Source = "opencost"

	if len(ocResp.Data) > 0 {
		for nsName, alloc := range ocResp.Data[0] {
			result.Namespaces = append(result.Namespaces, namespaceCost{
				Name:    nsName,
				Monthly: round2(alloc.TotalCost),
			})
			result.WorkloadCost += alloc.TotalCost
			result.TotalMonthly += alloc.TotalCost
		}
	}
	return result, nil
}

// --- Helpers ---

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

func sortWorkloads(w []workloadCost) {
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

func safePct(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return (used / total) * 100
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

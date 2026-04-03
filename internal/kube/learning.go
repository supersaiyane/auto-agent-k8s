package kube

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// LearningMode collects workload baselines over a learning period
// and auto-tunes thresholds per workload.
type LearningMode struct {
	mu        sync.RWMutex
	baselines map[string]*WorkloadBaseline // key: "ns/workload"
	period    time.Duration                // how long to learn (default 2 weeks)
	startTime time.Time
	savePath  string
}

// WorkloadBaseline tracks normal behavior for a workload.
type WorkloadBaseline struct {
	Workload       string    `json:"workload"`
	Namespace      string    `json:"namespace"`
	FirstSeen      time.Time `json:"firstSeen"`
	SampleCount    int       `json:"sampleCount"`
	CPUSamples     []float64 `json:"-"` // rolling window
	AvgCPU         float64   `json:"avgCpu"`
	StdDevCPU      float64   `json:"stdDevCpu"`
	AvgRestarts    float64   `json:"avgRestarts"`
	MaxRestarts    int32     `json:"maxRestarts"`
	NormalReplicas int32     `json:"normalReplicas"`
	// Computed thresholds
	CPUHighThreshold float64 `json:"cpuHighThreshold"` // avg + 2*stddev
	CPULowThreshold  float64 `json:"cpuLowThreshold"`  // avg - 1.5*stddev
}

func NewLearningMode(savePath string, period time.Duration) *LearningMode {
	if period <= 0 {
		period = 14 * 24 * time.Hour // 2 weeks default
	}
	if savePath == "" {
		savePath = "/var/log/auto-agent/baselines.json"
	}
	lm := &LearningMode{
		baselines: make(map[string]*WorkloadBaseline),
		period:    period,
		startTime: time.Now(),
		savePath:  savePath,
	}
	lm.load()
	return lm
}

// IsLearning returns true if still in the learning period.
func (lm *LearningMode) IsLearning() bool {
	return time.Since(lm.startTime) < lm.period
}

// RecordCPU adds a CPU sample for a workload.
func (lm *LearningMode) RecordCPU(ns, workload string, cpu float64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	key := ns + "/" + workload
	bl := lm.baselines[key]
	if bl == nil {
		bl = &WorkloadBaseline{
			Workload:  workload,
			Namespace: ns,
			FirstSeen: time.Now(),
		}
		lm.baselines[key] = bl
	}
	bl.SampleCount++
	bl.CPUSamples = append(bl.CPUSamples, cpu)
	// Keep rolling window of last 1000 samples
	if len(bl.CPUSamples) > 1000 {
		bl.CPUSamples = bl.CPUSamples[len(bl.CPUSamples)-1000:]
	}
	bl.AvgCPU = mean(bl.CPUSamples)
	bl.StdDevCPU = stddev(bl.CPUSamples, bl.AvgCPU)
	bl.CPUHighThreshold = bl.AvgCPU + 2*bl.StdDevCPU
	bl.CPULowThreshold = math.Max(0, bl.AvgCPU-1.5*bl.StdDevCPU)
}

// RecordRestarts adds a restart count observation.
func (lm *LearningMode) RecordRestarts(ns, workload string, restarts int32) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	key := ns + "/" + workload
	bl := lm.baselines[key]
	if bl == nil {
		return
	}
	if restarts > bl.MaxRestarts {
		bl.MaxRestarts = restarts
	}
}

// GetBaseline returns the baseline for a workload.
func (lm *LearningMode) GetBaseline(ns, workload string) *WorkloadBaseline {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	return lm.baselines[ns+"/"+workload]
}

// AllBaselines returns all baselines (for API).
func (lm *LearningMode) AllBaselines() map[string]*WorkloadBaseline {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	result := make(map[string]*WorkloadBaseline, len(lm.baselines))
	for k, v := range lm.baselines {
		result[k] = v
	}
	return result
}

// GetThreshold returns the learned CPU threshold for a workload,
// or the global default if no baseline exists.
func (lm *LearningMode) GetThreshold(ns, workload string, globalDefault float64) float64 {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	bl := lm.baselines[ns+"/"+workload]
	if bl == nil || bl.SampleCount < 50 {
		return globalDefault
	}
	if bl.CPUHighThreshold > 0 && bl.CPUHighThreshold < 1.0 {
		return bl.CPUHighThreshold
	}
	return globalDefault
}

// Save persists baselines to disk.
func (lm *LearningMode) Save() {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	data := make(map[string]*WorkloadBaseline, len(lm.baselines))
	for k, v := range lm.baselines {
		data[k] = v
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		klog.Warningf("learning: save failed: %v", err)
		return
	}
	os.MkdirAll("/var/log/auto-agent", 0o755)
	if err := os.WriteFile(lm.savePath, b, 0o644); err != nil {
		klog.Warningf("learning: write failed: %v", err)
		return
	}
	klog.V(3).Infof("learning: saved %d baselines to %s", len(data), lm.savePath)
}

func (lm *LearningMode) load() {
	b, err := os.ReadFile(lm.savePath)
	if err != nil {
		return // file doesn't exist yet
	}
	var data map[string]*WorkloadBaseline
	if err := json.Unmarshal(b, &data); err != nil {
		klog.Warningf("learning: load failed: %v", err)
		return
	}
	lm.baselines = data
	klog.Infof("learning: loaded %d baselines from %s", len(data), lm.savePath)
}

// CollectBaselines samples CPU for all deployments. Called periodically by leader.
func CollectBaselines(ctx context.Context, deps *Deps) {
	if deps.LearningMode == nil {
		return
	}
	for ns := range deps.Policy.NamespaceAllow {
		dl, err2 := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err2 != nil {
			continue
		}
		for i := range dl.Items {
			d := &dl.Items[i]
			cpu, err := deps.Metrics.AvgDeploymentCPU(ctx, d, "5m")
			if err != nil {
				continue
			}
			deps.LearningMode.RecordCPU(ns, d.Name, cpu)
		}
	}
	// Periodic save
	deps.LearningMode.Save()
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func stddev(vals []float64, avg float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	sumSq := 0.0
	for _, v := range vals {
		sumSq += (v - avg) * (v - avg)
	}
	return math.Sqrt(sumSq / float64(len(vals)-1))
}

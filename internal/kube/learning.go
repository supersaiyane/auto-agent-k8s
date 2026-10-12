package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// minLearnedSamples is how many CPU samples a baseline needs before it is
// trusted.
const minLearnedSamples = 50

// LearningMode collects workload baselines over a learning period
// and auto-tunes thresholds per workload.
type LearningMode struct {
	now       func() time.Time // injectable clock (PLAN-002 8.5); nil means time.Now
	mu        sync.RWMutex
	baselines map[string]*WorkloadBaseline // key: "ns/workload"
	period    time.Duration                // how long to learn (default 2 weeks)
	startTime time.Time
	savePath  string
}

// WorkloadBaseline tracks normal behavior for a workload.
type WorkloadBaseline struct {
	Workload    string    `json:"workload"`
	Namespace   string    `json:"namespace"`
	FirstSeen   time.Time `json:"firstSeen"`
	SampleCount int       `json:"sampleCount"`
	CPUSamples  []float64 `json:"-"` // rolling window
	AvgCPU      float64   `json:"avgCpu"`
	StdDevCPU   float64   `json:"stdDevCpu"`
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
	return lm.clock().Sub(lm.startTime) < lm.period
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
			FirstSeen: lm.clock(),
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

// GetBaseline returns a copy of the baseline for a workload, or nil.
func (lm *LearningMode) GetBaseline(ns, workload string) *WorkloadBaseline {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	bl := lm.baselines[ns+"/"+workload]
	if bl == nil {
		return nil
	}
	c := *bl
	c.CPUSamples = append([]float64(nil), bl.CPUSamples...)
	return &c
}

// AllBaselines returns a copy of every baseline, for the API. The copies
// are taken under the lock, so encoding them cannot race with RecordCPU
// (ISS-096).
func (lm *LearningMode) AllBaselines() map[string]*WorkloadBaseline {
	lm.mu.RLock()
	defer lm.mu.RUnlock()
	result := make(map[string]*WorkloadBaseline, len(lm.baselines))
	for k, v := range lm.baselines {
		c := *v
		c.CPUSamples = nil // not served (json:"-"), and the live slice must not escape
		result[k] = &c
	}
	return result
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
		learningFailed("save", lm.savePath, err)
		return
	}
	// The directory of the configured path, not a fixed one (ISS-097).
	if err := os.MkdirAll(filepath.Dir(lm.savePath), 0o755); err != nil {
		learningFailed("save", lm.savePath, err)
		return
	}
	if err := os.WriteFile(lm.savePath, b, 0o644); err != nil {
		learningFailed("save", lm.savePath, err)
		return
	}
	klog.V(3).Infof("learning: saved %d baselines to %s", len(data), lm.savePath)
}

func (lm *LearningMode) load() {
	b, err := os.ReadFile(lm.savePath)
	if errors.Is(err, fs.ErrNotExist) {
		return // first start
	}
	if err != nil {
		learningFailed("load", lm.savePath, err)
		return
	}
	var data map[string]*WorkloadBaseline
	if err := json.Unmarshal(b, &data); err != nil {
		learningFailed("load", lm.savePath, err)
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
	for _, ns := range watchedNamespaces(ctx, deps) {
		dl, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "deployments", ns) // phase 12: was dropped
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

// clock returns the current time from the injected clock, or time.Now.
func (lm *LearningMode) clock() time.Time {
	if lm.now != nil {
		return lm.now()
	}
	return time.Now()
}

// Bounds on a learned scale-up threshold. The floor stays well above the
// 0.3 scale-down line, so a quiet workload's low baseline can neither make
// the scaler react to noise nor make scale up and scale down overlap.
const (
	learnedThresholdMin = 0.5
	learnedThresholdMax = 0.95
)

// scaleUpThreshold is the CPU ratio that triggers a scale up for one
// Deployment and where it came from: the learned baseline (mean plus two
// standard deviations, once 50 samples exist), clamped to
// [learnedThresholdMin, learnedThresholdMax], or the global setting
// (ISS-012).
func scaleUpThreshold(deps *Deps, ns, name string, global float64) (float64, string) {
	if deps.LearningMode == nil {
		return global, "global"
	}
	bl := deps.LearningMode.GetBaseline(ns, name)
	if bl == nil || bl.SampleCount < minLearnedSamples || bl.CPUHighThreshold <= 0 {
		return global, "global"
	}
	return math.Min(math.Max(bl.CPUHighThreshold, learnedThresholdMin), learnedThresholdMax), "learned"
}

// learningFailed logs and counts a failure to save or load baselines.
func learningFailed(op, path string, err error) {
	klog.Warningf("learning: cannot %s %s: %v", op, path, err)
	obs.HandlerErrorsTotal.WithLabelValues("learning", op).Inc()
}

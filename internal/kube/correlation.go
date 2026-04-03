package kube

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// DeployTracker tracks recent deployments to correlate with incidents.
type DeployTracker struct {
	mu      sync.RWMutex
	deploys []DeployRecord
	maxLen  int
}

type DeployRecord struct {
	Timestamp  time.Time `json:"timestamp"`
	Namespace  string    `json:"namespace"`
	Deployment string    `json:"deployment"`
	Revision   int64     `json:"revision"`
	Image      string    `json:"image"`
	Replicas   int32     `json:"replicas"`
}

func NewDeployTracker(maxLen int) *DeployTracker {
	if maxLen <= 0 {
		maxLen = 100
	}
	return &DeployTracker{deploys: make([]DeployRecord, 0, maxLen), maxLen: maxLen}
}

// RecordDeploy tracks a deployment change.
func (dt *DeployTracker) RecordDeploy(ns, name, image string, revision int64, replicas int32) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	if len(dt.deploys) >= dt.maxLen {
		dt.deploys = dt.deploys[1:]
	}
	dt.deploys = append(dt.deploys, DeployRecord{
		Timestamp: time.Now().UTC(), Namespace: ns, Deployment: name,
		Image: image, Revision: revision, Replicas: replicas,
	})
}

// RecentDeploys returns deploys in the last N minutes for a namespace.
func (dt *DeployTracker) RecentDeploys(ns string, within time.Duration) []DeployRecord {
	dt.mu.RLock()
	defer dt.mu.RUnlock()
	cutoff := time.Now().Add(-within)
	var result []DeployRecord
	for i := len(dt.deploys) - 1; i >= 0; i-- {
		d := dt.deploys[i]
		if d.Timestamp.Before(cutoff) {
			break
		}
		if ns == "" || d.Namespace == ns {
			result = append(result, d)
		}
	}
	return result
}

// All returns all tracked deploys (for API).
func (dt *DeployTracker) All() []DeployRecord {
	dt.mu.RLock()
	defer dt.mu.RUnlock()
	return append([]DeployRecord(nil), dt.deploys...)
}

// CorrelateIncident checks if an incident may be caused by a recent deploy.
func (dt *DeployTracker) CorrelateIncident(ns, workload string) string {
	recent := dt.RecentDeploys(ns, 10*time.Minute)
	if len(recent) == 0 {
		return ""
	}
	for _, d := range recent {
		return fmt.Sprintf("_Correlation_: deploy `%s` (image: `%s`) happened %s ago — may be the cause.\n",
			d.Deployment, d.Image, time.Since(d.Timestamp).Round(time.Second))
	}
	return ""
}

// ScanDeployments checks for new/changed deployments and records them.
func ScanDeployments(ctx context.Context, deps *Deps) {
	if deps.DeployTracker == nil {
		return
	}
	for ns := range deps.Policy.NamespaceAllow {
		deploys, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, d := range deploys.Items {
			rev := parseDeployRevision(&d)
			image := ""
			if len(d.Spec.Template.Spec.Containers) > 0 {
				image = d.Spec.Template.Spec.Containers[0].Image
			}
			replicas := int32(1)
			if d.Spec.Replicas != nil {
				replicas = *d.Spec.Replicas
			}
			// Only record if we haven't seen this revision
			if !deps.DeployTracker.hasRevision(ns, d.Name, rev) {
				deps.DeployTracker.RecordDeploy(ns, d.Name, image, rev, replicas)
				klog.V(4).Infof("correlation: tracked deploy %s/%s rev=%d", ns, d.Name, rev)
			}
		}
	}
}

func (dt *DeployTracker) hasRevision(ns, name string, rev int64) bool {
	dt.mu.RLock()
	defer dt.mu.RUnlock()
	for _, d := range dt.deploys {
		if d.Namespace == ns && d.Deployment == name && d.Revision == rev {
			return true
		}
	}
	return false
}

func parseDeployRevision(d *appsv1.Deployment) int64 {
	if d.Annotations == nil {
		return 0
	}
	var rev int64
	fmt.Sscanf(d.Annotations["deployment.kubernetes.io/revision"], "%d", &rev)
	return rev
}

// CostEstimate returns a rough monthly cost estimate for a deployment.
// Uses a simple formula: replicas * (cpu_requests * cpu_price + mem_requests * mem_price).
func CostEstimate(cpuRequests float64, memRequestsMi float64, replicas int32) string {
	// Average cloud pricing (rough): $0.05/cpu-hour, $0.005/GiB-hour
	cpuPricePerHour := 0.05
	memPricePerGiBHour := 0.005
	hoursPerMonth := 730.0

	cpuCost := cpuRequests * cpuPricePerHour * hoursPerMonth * float64(replicas)
	memCost := (memRequestsMi / 1024.0) * memPricePerGiBHour * hoursPerMonth * float64(replicas)
	total := cpuCost + memCost

	return fmt.Sprintf("~$%.0f/month (%d replicas x %.1f CPU + %.0f Mi)", total, replicas, cpuRequests, memRequestsMi)
}

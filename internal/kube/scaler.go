package kube

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

const (
	annoLastScaleUp   = "auto-agent.io/last-scale-ts"
	annoLastScaleDown = "auto-agent.io/last-scale-down-ts"
)

// EvaluateAndScale runs the scaling loop for all allowed namespaces.
// Must be called only by the leader.
func EvaluateAndScale(ctx context.Context, deps *Deps) {
	pol := deps.Policy

	cooldownUp := parseDuration(pol.CooldownUp, "2m")
	cooldownDown := parseDuration(pol.CooldownDown, "10m")

	for ns := range pol.NamespaceAllow {
		dl, err := deps.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.V(2).Infof("scaler: failed to list deployments in %s: %v", ns, err)
			continue
		}

		for i := range dl.Items {
			d := &dl.Items[i]
			rep := int32(1)
			if d.Spec.Replicas != nil {
				rep = *d.Spec.Replicas
			}

			// HPA coexistence check: skip if HPA exists for this deployment
			if pol.HPACoexistence {
				hasHPA, err := deploymentHasHPA(ctx, deps, ns, d.Name)
				if err != nil {
					klog.V(3).Infof("scaler: HPA check failed for %s/%s: %v", ns, d.Name, err)
				}
				if hasHPA {
					klog.V(4).Infof("scaler: skipping %s/%s (HPA present)", ns, d.Name)
					continue
				}
			}

			// Check scale-up cooldown
			if inCooldown(d.Annotations, annoLastScaleUp, cooldownUp) {
				continue
			}

			// Query CPU
			cpu, err := deps.Metrics.AvgDeploymentCPU(ctx, d, pol.ScaleWindow)
			if err != nil {
				klog.V(3).Infof("scaler: CPU query failed for %s/%s: %v", ns, d.Name, err)
				continue
			}

			// Evaluate gate signals
			gateActive := evaluateGates(ctx, deps)

			// --- Scale Up ---
			if cpu > pol.CPUThreshold && gateActive {
				// Determine max replicas (from CRD policy or global)
				maxRep := pol.MaxReplicas
				crdPolicies := deps.CRDStore.Match(ns, d.Spec.Template.Labels)
				for _, cp := range crdPolicies {
					if cp.Scale.Enabled && cp.Scale.MaxReplicas > 0 {
						maxRep = cp.Scale.MaxReplicas
						break
					}
				}

				if rep >= maxRep {
					klog.V(2).Infof("scaler: %s/%s at max replicas (%d)", ns, d.Name, maxRep)
					continue
				}

				step := int32(pol.MaxScaleStep)
				newRep := rep + step
				if newRep > maxRep {
					newRep = maxRep
				}

				d.Spec.Replicas = &newRep
				if d.Annotations == nil {
					d.Annotations = map[string]string{}
				}
				d.Annotations[annoLastScaleUp] = time.Now().UTC().Format(time.RFC3339)

				if _, err := deps.Client.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
					klog.Warningf("scaler: failed to scale up %s/%s: %v", ns, d.Name, err)
					obs.HandlerErrorsTotal.WithLabelValues("scaler", "scale_up").Inc()
					continue
				}

				msg := fmt.Sprintf("*ScaleUp*: `%s/%s` %d -> %d (cpu=%.2f, gate=%t)", ns, d.Name, rep, newRep, cpu, gateActive)
				klog.Infof("scaler: %s", msg)
				deps.Slack.Post(msg)
				obs.ActionsTotal.WithLabelValues("scale_up", ns, d.Name).Inc()
				obs.ScalingDecisionsTotal.WithLabelValues("up", ns, d.Name).Inc()
				continue
			}

			// --- Scale Down ---
			if cpu < 0.3 && !gateActive && rep > pol.MinReplicas {
				// Check scale-down cooldown (longer than scale-up)
				if inCooldown(d.Annotations, annoLastScaleDown, cooldownDown) {
					continue
				}

				newRep := rep - 1
				if newRep < pol.MinReplicas {
					newRep = pol.MinReplicas
				}
				if newRep == rep {
					continue
				}

				d.Spec.Replicas = &newRep
				if d.Annotations == nil {
					d.Annotations = map[string]string{}
				}
				d.Annotations[annoLastScaleDown] = time.Now().UTC().Format(time.RFC3339)

				if _, err := deps.Client.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
					klog.Warningf("scaler: failed to scale down %s/%s: %v", ns, d.Name, err)
					obs.HandlerErrorsTotal.WithLabelValues("scaler", "scale_down").Inc()
					continue
				}

				msg := fmt.Sprintf("*ScaleDown*: `%s/%s` %d -> %d (cpu=%.2f)", ns, d.Name, rep, newRep, cpu)
				klog.Infof("scaler: %s", msg)
				deps.Slack.Post(msg)
				obs.ActionsTotal.WithLabelValues("scale_down", ns, d.Name).Inc()
				obs.ScalingDecisionsTotal.WithLabelValues("down", ns, d.Name).Inc()
			}
		}
	}
}

// evaluateGates checks optional PromQL gate signals. Returns true if any gate
// is active (indicating real load), or if no gates are configured (CPU-only mode).
func evaluateGates(ctx context.Context, deps *Deps) bool {
	qDepthQ := os.Getenv("PROM_QUEUE_DEPTH")
	errRateQ := os.Getenv("PROM_ERROR_RATE")
	p95Q := os.Getenv("PROM_P95_LATENCY")

	// No gates configured: fall back to CPU-only mode
	if qDepthQ == "" && errRateQ == "" && p95Q == "" {
		return true
	}

	if qDepthQ != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, qDepthQ); err == nil && v > 0 {
			return true
		}
	}
	if errRateQ != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, errRateQ); err == nil && v > 0 {
			return true
		}
	}
	if p95Q != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, p95Q); err == nil && v > 0 {
			return true
		}
	}
	return false
}

// deploymentHasHPA checks if a HorizontalPodAutoscaler targets this deployment.
func deploymentHasHPA(ctx context.Context, deps *Deps, ns, deploymentName string) (bool, error) {
	hpaList, err := deps.Client.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("list HPAs: %w", err)
	}
	for _, hpa := range hpaList.Items {
		ref := hpa.Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == deploymentName {
			return true, nil
		}
	}
	return false, nil
}

func inCooldown(annotations map[string]string, key string, cooldown time.Duration) bool {
	if annotations == nil {
		return false
	}
	ts, ok := annotations[key]
	if !ok {
		return false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return time.Since(t) < cooldown
}

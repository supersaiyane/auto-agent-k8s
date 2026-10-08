package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/crd"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

const (
	annoLastScaleUp   = "auto-agent.io/last-scale-ts"
	annoLastScaleDown = "auto-agent.io/last-scale-down-ts"
)

// EvaluateAndScale runs the scaling loop for all allowed namespaces.
// Must be called only by the leader.
func EvaluateAndScale(ctx context.Context, deps *Deps) {
	pol := deps.Policy()

	for _, ns := range watchedNamespaces(ctx, deps) {
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

			// The AutoRemediationPolicy that selects this Deployment's pods
			// tunes its limits (ISS-037).
			cp := effectivePolicy(deps, ns, d.Spec.Template.Labels)
			lim := scaleLimitsFor(pol, cp)

			// HPA coexistence check: skip if HPA exists for this deployment,
			// unless its policy sets scale.allowHPAOverride.
			if pol.HPACoexistence && !lim.overrideHPA {
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
			if inCooldown(d.Annotations, annoLastScaleUp, lim.cooldownUp) {
				continue
			}

			// Query CPU
			cpu, err := deps.Metrics.AvgDeploymentCPU(ctx, d, pol.ScaleWindow)
			if err != nil {
				klog.V(3).Infof("scaler: CPU query failed for %s/%s: %v", ns, d.Name, err)
				continue
			}

			// Evaluate gate signals
			gatesConfigured, gateActive := evaluateGates(ctx, deps)

			// --- Scale Up ---
			threshold, source := scaleUpThreshold(deps, ns, d.Name, pol.CPUThreshold)
			if cpu > threshold && gateActive {
				maxRep := lim.max
				if rep >= maxRep {
					klog.V(2).Infof("scaler: %s/%s at max replicas (%d)", ns, d.Name, maxRep)
					continue
				}

				newRep := rep + lim.step
				if newRep > maxRep {
					newRep = maxRep
				}

				if outcome, gmsg := scaleDeployment(ctx, deps, d, rep, newRep, annoLastScaleUp, "ScaleUp", "scale_up"); outcome != gateApplied {
					klog.V(2).Infof("scaler: scale up %s/%s not applied: %s", ns, d.Name, strings.TrimSpace(gmsg))
					continue
				}

				msg := fmt.Sprintf("*ScaleUp*: `%s/%s` %d -> %d (cpu=%.2f over the %s threshold %.2f, gate=%t)", ns, d.Name, rep, newRep, cpu, source, threshold, gateActive)
				klog.Infof("scaler: %s", msg)
				postSlack(deps, msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Scaling, Severity: eventsvc.SevInfo,
					Namespace: ns, Workload: d.Name, Reason: "ScaleUp",
					Message: fmt.Sprintf("%d -> %d replicas (cpu=%.2f over the %s threshold %.2f)", rep, newRep, cpu, source, threshold)})
				obs.ScalingDecisionsTotal.WithLabelValues("up", ns, d.Name).Inc()
				continue
			}

			// --- Scale Down ---
			// Scale down if CPU is low AND either no gates configured (CPU-only) or gates show no load
			canScaleDown := !gatesConfigured || !gateActive
			if cpu < 0.3 && canScaleDown && rep > lim.min {
				// Check scale-down cooldown (longer than scale-up)
				if inCooldown(d.Annotations, annoLastScaleDown, lim.cooldownDown) {
					continue
				}

				newRep := rep - 1
				if newRep < lim.min {
					newRep = lim.min
				}
				if newRep == rep {
					continue
				}

				if outcome, gmsg := scaleDeployment(ctx, deps, d, rep, newRep, annoLastScaleDown, "ScaleDown", "scale_down"); outcome != gateApplied {
					klog.V(2).Infof("scaler: scale down %s/%s not applied: %s", ns, d.Name, strings.TrimSpace(gmsg))
					continue
				}

				msg := fmt.Sprintf("*ScaleDown*: `%s/%s` %d -> %d (cpu=%.2f)", ns, d.Name, rep, newRep, cpu)
				klog.Infof("scaler: %s", msg)
				postSlack(deps, msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Scaling, Severity: eventsvc.SevInfo,
					Namespace: ns, Workload: d.Name, Reason: "ScaleDown",
					Message: fmt.Sprintf("%d -> %d replicas (cpu=%.2f)", rep, newRep, cpu)})
				obs.ScalingDecisionsTotal.WithLabelValues("down", ns, d.Name).Inc()
			}
		}
	}
}

// evaluateGates checks optional PromQL gate signals.
// Returns (gatesConfigured, gateActive).
// - gatesConfigured: true if any PROM_* env vars are set
// - gateActive: true if any gate signal indicates real load, OR if no gates configured (CPU-only mode)
func evaluateGates(ctx context.Context, deps *Deps) (bool, bool) {
	qDepthQ := deps.ScalingGates.QueueDepth
	errRateQ := deps.ScalingGates.ErrorRate
	p95Q := deps.ScalingGates.P95Latency

	// No gates configured: CPU-only mode
	if qDepthQ == "" && errRateQ == "" && p95Q == "" {
		return false, true // not configured, active (allow scale-up on CPU alone)
	}

	if qDepthQ != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, qDepthQ); err == nil && v > 0 {
			return true, true
		}
	}
	if errRateQ != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, errRateQ); err == nil && v > 0 {
			return true, true
		}
	}
	if p95Q != "" {
		if v, err := deps.Metrics.QueryInstant(ctx, p95Q); err == nil && v > 0 {
			return true, true
		}
	}
	return true, false // configured but inactive (no load signals)
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

// scaleDeployment sets the replica count and cooldown annotation with one
// merge patch sent through the mutation gate.
func scaleDeployment(ctx context.Context, deps *Deps, d *appsv1.Deployment, from, to int32,
	cooldownAnno, reason, actionType string) (gateOutcome, string) {

	patch := mergePatch(map[string]any{
		"spec":     map[string]any{"replicas": to},
		"metadata": map[string]any{"annotations": map[string]any{cooldownAnno: time.Now().UTC().Format(time.RFC3339)}},
	})
	return applyMutation(ctx, deps, mutation{
		Namespace: d.Namespace, Workload: d.Name, Labels: d.Spec.Template.Labels,
		Reason: reason, ActionType: actionType,
		SuccessMsg: fmt.Sprintf("scaled %s/%s from %d to %d replicas", d.Namespace, d.Name, from, to),
		SuggestMsg: fmt.Sprintf("scale %s/%s from %d to %d replicas", d.Namespace, d.Name, from, to),
		Apply: func() error {
			_, err := deps.Client.AppsV1().Deployments(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{})
			return err
		},
	})
}

// scaleLimits are the bounds the scaler uses for one Deployment.
type scaleLimits struct {
	min, max, step           int32
	cooldownUp, cooldownDown time.Duration
	overrideHPA              bool
}

// scaleLimitsFor applies an AutoRemediationPolicy's scale and cooldown
// settings over the global policy (ISS-037). Scale fields count only with
// scale.enabled; safety.cooldown applies to both directions. A policy
// minimum above its maximum is ignored rather than trusted.
func scaleLimitsFor(pol *policy.Policy, cp *crd.Policy) scaleLimits {
	l := scaleLimits{min: pol.MinReplicas, max: pol.MaxReplicas, step: int32(pol.MaxScaleStep)}
	l.cooldownUp, l.cooldownDown = effectiveCooldown(cp, pol.CooldownUp, pol.CooldownDown)
	if cp == nil || !cp.Scale.Enabled {
		return l
	}
	sc := cp.Scale
	if sc.MaxReplicas > 0 {
		l.max = sc.MaxReplicas
	}
	if sc.MinReplicas > 0 && sc.MinReplicas <= l.max {
		l.min = sc.MinReplicas
	}
	if sc.Step > 0 {
		l.step = sc.Step
	}
	l.overrideHPA = sc.AllowHPAOverride
	return l
}

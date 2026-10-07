package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func handleNodePressure(ctx context.Context, deps *Deps, oldNode, newNode *corev1.Node) {
	// One actor per target (CLAUDE.md constraint 3): every DaemonSet pod sees
	// every node event, so only the agent running on this node acts on it.
	if deps.NodeName == "" || newNode.Name != deps.NodeName {
		klog.V(4).Infof("handler: node %s is not ours (%q), skipping", newNode.Name, deps.NodeName)
		return
	}

	var memP, diskP bool
	for _, c := range newNode.Status.Conditions {
		if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
			memP = true
		}
		if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue {
			diskP = true
		}
	}

	// --- Pressure RESOLVED: uncordon the node, if we were the ones who cordoned it ---
	if !(memP || diskP) {
		if newNode.Spec.Unschedulable && newNode.Annotations["auto-agent.io/cordoned"] == "true" {
			patch := mergePatch(map[string]any{
				"spec":     map[string]any{"unschedulable": false},
				"metadata": map[string]any{"annotations": map[string]any{"auto-agent.io/cordoned": nil}},
			})
			_, gmsg := applyMutation(ctx, deps, mutation{
				Workload: newNode.Name, Reason: "NodePressureResolved", ActionType: "uncordon_node",
				SuccessMsg: "uncordoned node", SuggestMsg: "uncordon node",
				Apply: func() error {
					_, err := deps.Client.CoreV1().Nodes().Patch(ctx, newNode.Name, types.MergePatchType, patch, metav1.PatchOptions{})
					return err
				},
			})
			if gmsg != "" {
				if err := deps.Slack.Postf("*NodePressure resolved* on `%s`\n%s", newNode.Name, gmsg); err != nil {
					obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "slack").Inc()
				}
			}
		}
		return
	}

	// --- Pressure ACTIVE ---
	klog.Infof("handler: node pressure detected on %s (memory:%t disk:%t)", newNode.Name, memP, diskP)

	key := dedupKey("", newNode.Name, "NodePressure")
	if !deps.Dedup.Check(key) {
		obs.DedupSkippedTotal.WithLabelValues("NodePressure").Inc()
		return
	}

	msg := fmt.Sprintf("*NodePressure* detected on `%s` (memory:%t disk:%t)\n", newNode.Name, memP, diskP)

	// Observe and suggest never reach the gate per pod: one suggestion is enough.
	if deps.Policy().Mode != policy.Fix && deps.Policy().Mode != policy.DryRun {
		msg += "_Suggest_: cordon node and evict non-critical pods.\n"
		deps.Slack.Post(msg)
		obs.IncidentsTotal.WithLabelValues("NodePressure", "", newNode.Name).Inc()
		return
	}

	// Cordon the node with our annotation marker
	if !newNode.Spec.Unschedulable {
		patch := mergePatch(map[string]any{
			"spec":     map[string]any{"unschedulable": true},
			"metadata": map[string]any{"annotations": map[string]any{"auto-agent.io/cordoned": "true"}},
		})
		_, gmsg := applyMutation(ctx, deps, mutation{
			Workload: newNode.Name, Reason: "NodePressure", ActionType: "cordon_node",
			SuccessMsg: "cordoned node (marked unschedulable)", SuggestMsg: "cordon node",
			Apply: func() error {
				_, err := deps.Client.CoreV1().Nodes().Patch(ctx, newNode.Name, types.MergePatchType, patch, metav1.PatchOptions{})
				return err
			},
		})
		msg += gmsg
	}

	// Evict non-critical pods with rate limiting and PDB awareness
	pl, err := deps.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", newNode.Name).String(),
	})
	if err != nil {
		klog.Warningf("handler: failed to list pods on node %s: %v", newNode.Name, err)
		obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "list_pods").Inc()
	} else {
		evicted, simulated := 0, 0
		for i := range pl.Items {
			p := &pl.Items[i]
			if !deps.Policy().AllowedNamespace(p.Namespace) || isCriticalPod(p) {
				continue
			}
			if isStatefulSetPod(p) {
				klog.V(2).Infof("handler: skipping StatefulSet pod %s/%s during eviction", p.Namespace, p.Name)
				continue
			}
			gr := int64(30)
			ev := &policyv1.Eviction{
				ObjectMeta:    metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name},
				DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &gr},
			}
			outcome, gmsg := applyMutation(ctx, deps, mutation{
				Namespace: p.Namespace, Workload: ownerName(p), Pod: p.Name, Labels: p.Labels,
				Reason: "NodePressure", ActionType: "evict_pod",
				SuccessMsg: "evicted pod", SuggestMsg: "evict pod",
				Apply: func() error { return deps.Client.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev) },
			})
			switch outcome {
			case gateApplied:
				evicted++
			case gateSimulated:
				simulated++
			}
			if outcome == gateBlocked {
				msg += gmsg
				break
			}
		}
		if simulated > 0 {
			msg += fmt.Sprintf("_DryRun_: would evict %d non-critical pods.\n", simulated)
		}
		if evicted > 0 {
			msg += fmt.Sprintf("_Action_: evicted %d non-critical pods.\n", evicted)
		}
	}

	deps.Slack.Post(msg)
	obs.IncidentsTotal.WithLabelValues("NodePressure", "", newNode.Name).Inc()
}

package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
	"github.com/yourorg/auto-agent/internal/policy"
)

func handleNodePressure(ctx context.Context, deps *Deps, node *corev1.Node) {
	var memP, diskP bool
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
			memP = true
		}
		if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue {
			diskP = true
		}
	}
	if !(memP || diskP) {
		return
	}

	klog.Infof("handler: node pressure detected on %s (memory:%t disk:%t)", node.Name, memP, diskP)

	// Dedup per node
	key := dedupKey("", node.Name, "NodePressure")
	if !deps.Dedup.Check(key) {
		obs.DedupSkippedTotal.WithLabelValues("NodePressure").Inc()
		return
	}

	msg := fmt.Sprintf("*NodePressure* detected on `%s` (memory:%t disk:%t)\n", node.Name, memP, diskP)

	if deps.Policy.Mode != policy.Fix {
		msg += "_Suggest_: cordon node and evict non-critical pods.\n"
		if err := deps.Slack.Post(msg); err != nil {
			obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "slack").Inc()
		}
		obs.IncidentsTotal.WithLabelValues("NodePressure", "", node.Name).Inc()
		return
	}

	// Cordon the node
	if !node.Spec.Unschedulable {
		ncopy := node.DeepCopy()
		ncopy.Spec.Unschedulable = true
		if _, err := deps.Client.CoreV1().Nodes().Update(ctx, ncopy, metav1.UpdateOptions{}); err != nil {
			klog.Warningf("handler: failed to cordon node %s: %v", node.Name, err)
			obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "cordon").Inc()
			msg += fmt.Sprintf("_Action_: failed to cordon: %v\n", err)
		} else {
			msg += "_Action_: cordoned node (marked unschedulable).\n"
			obs.ActionsTotal.WithLabelValues("cordon_node", "", node.Name).Inc()
		}
	}

	// Evict non-critical pods with rate limiting and PDB awareness
	pl, err := deps.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node.Name).String(),
	})
	if err != nil {
		klog.Warningf("handler: failed to list pods on node %s: %v", node.Name, err)
		obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "list_pods").Inc()
	} else {
		evicted := 0
		for _, p := range pl.Items {
			if isCriticalPod(&p) {
				continue
			}
			// Don't evict StatefulSet pods during pressure — they need ordered lifecycle
			if isStatefulSetPod(&p) {
				klog.V(2).Infof("handler: skipping StatefulSet pod %s/%s during eviction", p.Namespace, p.Name)
				continue
			}

			if !deps.Limiter.Allow() {
				obs.RateLimitedTotal.Inc()
				klog.Warningf("handler: rate limit reached during node eviction on %s", node.Name)
				break
			}

			gr := int64(30)
			ev := &policyv1.Eviction{
				ObjectMeta:    metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name},
				DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: &gr},
			}
			if err := deps.Client.PolicyV1().Evictions(p.Namespace).Evict(ctx, ev); err != nil {
				// PDB violation will return 429 Too Many Requests — respect it
				klog.V(2).Infof("handler: eviction of %s/%s failed (may be PDB-protected): %v", p.Namespace, p.Name, err)
				continue
			}
			obs.ActionsTotal.WithLabelValues("evict_pod", p.Namespace, ownerName(&p)).Inc()
			evicted++
		}
		msg += fmt.Sprintf("_Action_: evicted %d non-critical pods.\n", evicted)
	}

	if err := deps.Slack.Post(msg); err != nil {
		obs.HandlerErrorsTotal.WithLabelValues("nodepressure", "slack").Inc()
	}
	obs.IncidentsTotal.WithLabelValues("NodePressure", "", node.Name).Inc()
}

package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

const quotaUsageThreshold = 0.9 // alert at 90% usage

// CheckResourceQuotas scans allowed namespaces for resource quotas nearing exhaustion.
// Must be called only by the leader.
func CheckResourceQuotas(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		quotas, err := deps.Client.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			klog.V(3).Infof("quotas: failed to list in %s: %v", ns, err)
			continue
		}

		for _, rq := range quotas.Items {
			checkQuota(ctx, deps, ns, &rq)
		}
	}
}

func checkQuota(ctx context.Context, deps *Deps, ns string, rq *corev1.ResourceQuota) {
	for resource, hard := range rq.Status.Hard {
		used, ok := rq.Status.Used[resource]
		if !ok {
			continue
		}

		hardVal := hard.AsApproximateFloat64()
		usedVal := used.AsApproximateFloat64()
		if hardVal <= 0 {
			continue
		}

		ratio := usedVal / hardVal
		if ratio < quotaUsageThreshold {
			continue
		}

		key := dedupKey(ns, rq.Name, fmt.Sprintf("quota-%s", resource))
		if !deps.Dedup.Check(key) {
			obs.DedupSkippedTotal.WithLabelValues("QuotaExhaustion").Inc()
			continue
		}

		pct := ratio * 100
		msg := fmt.Sprintf("*ResourceQuota* warning in `%s`\nQuota: `%s` resource: `%s`\nUsed: %s / %s (%.0f%%)\n",
			ns, rq.Name, resource, used.String(), hard.String(), pct)

		if ratio >= 1.0 {
			msg += "_Status_: EXHAUSTED. New pods requesting this resource will fail to schedule.\n"
		} else {
			msg += fmt.Sprintf("_Status_: approaching limit (>%.0f%%). Consider increasing quota or reducing usage.\n",
				quotaUsageThreshold*100)
		}

		klog.Infof("quotas: %s/%s resource %s at %.0f%%", ns, rq.Name, resource, pct)

		if err := deps.Slack.Post(msg); err != nil {
			obs.HandlerErrorsTotal.WithLabelValues("quota", "slack").Inc()
		}
		obs.IncidentsTotal.WithLabelValues("QuotaExhaustion", ns, string(resource)).Inc()
	}
}

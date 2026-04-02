package kube

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckAnomalies evaluates CRD-driven anomaly PromQL rules.
// Must be called only by the leader.
func CheckAnomalies(ctx context.Context, deps *Deps) {
	namespaces := deps.CRDStore.AllNamespaces()
	if len(namespaces) == 0 {
		klog.V(4).Infof("anomalies: no CRD policies found, skipping")
		return
	}

	for _, ns := range namespaces {
		policies := deps.CRDStore.List(ns)
		for _, pol := range policies {
			for _, rule := range pol.Anomalies {
				if rule.PromQL == "" {
					continue
				}

				val, err := deps.Metrics.QueryInstant(ctx, rule.PromQL)
				if err != nil {
					klog.V(3).Infof("anomalies: query failed for %s/%s rule %s: %v", ns, pol.Name, rule.Name, err)
					continue
				}

				threshold := rule.ZScoreThreshold
				if threshold <= 0 {
					threshold = 1.0 // default threshold
				}

				if val > threshold {
					key := dedupKey(ns, pol.Name, "anomaly-"+rule.Name)
					if !deps.Dedup.Check(key) {
						obs.DedupSkippedTotal.WithLabelValues("anomaly").Inc()
						continue
					}

					msg := fmt.Sprintf("*Anomaly* detected by policy `%s/%s` rule `%s`\nPromQL: `%s`\nValue: %.4f (threshold: %.4f)\n",
						ns, pol.Name, rule.Name, rule.PromQL, val, threshold)

					if pol.RunbookURL != "" {
						msg += fmt.Sprintf("Runbook: %s\n", pol.RunbookURL)
					}

					klog.Infof("anomalies: %s/%s rule %s triggered (val=%.4f > threshold=%.4f)", ns, pol.Name, rule.Name, val, threshold)

					if err := deps.Slack.Post(msg); err != nil {
						obs.HandlerErrorsTotal.WithLabelValues("anomaly", "slack").Inc()
					}
					obs.AnomaliesDetectedTotal.WithLabelValues(pol.Name, ns, rule.Name).Inc()
				}
			}
		}
	}
}

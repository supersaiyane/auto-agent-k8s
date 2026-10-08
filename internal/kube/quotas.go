package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

const quotaUsageThreshold = 0.9 // report at 90% usage

// CheckResourceQuotas reports quota resources at or above
// quotaUsageThreshold, once an hour each. The phase 12 audit found it sent
// Slack only: no event (so the dashboard never showed it), no rung, and a
// failed list was dropped. Rung R1; R3 (raise within a ceiling) arrives
// with the approval queue.
// Must be called only by the leader.
func CheckResourceQuotas(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		quotas, err := deps.Client.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "resourcequotas", ns)
			continue
		}
		for i := range quotas.Items {
			for _, f := range quotaFindings(&quotas.Items[i]) {
				if deps.Dedup.CheckFor(dedupKey(ns, f.Workload, "quota-"+f.Subject), time.Hour) {
					report(ctx, deps, f)
				}
			}
		}
	}
}

func quotaFindings(rq *corev1.ResourceQuota) []finding {
	var names []string
	for r := range rq.Status.Hard {
		names = append(names, string(r))
	}
	sort.Strings(names)
	var out []finding
	for _, name := range names {
		hard := rq.Status.Hard[corev1.ResourceName(name)]
		used, ok := rq.Status.Used[corev1.ResourceName(name)]
		if !ok || hard.AsApproximateFloat64() <= 0 {
			continue
		}
		ratio := used.AsApproximateFloat64() / hard.AsApproximateFloat64()
		if ratio < quotaUsageThreshold {
			continue
		}
		f := finding{Reason: "QuotaExhaustion", Namespace: rq.Namespace, Workload: "resourcequota/" + rq.Name, Subject: name,
			Severity: eventsvc.SevWarning, Rung: RungGuided,
			Summary: fmt.Sprintf("`%s` at %.0f%%: %s of %s used", name, ratio*100, used.String(), hard.String()),
			Fix:     "raise the quota if the usage is expected, or free the resource; new pods needing it are refused at 100%"}
		if ratio >= 1 {
			f.Severity = eventsvc.SevCritical
			f.Summary += ", exhausted"
		}
		out = append(out, f)
	}
	return out
}

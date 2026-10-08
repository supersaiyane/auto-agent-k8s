package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// Windows for deletion and disruption checks (PLAN-002 10.2, 10.6).
const (
	finalizerStuckAfter = 15 * time.Minute // deleting, but finalizers still hold the object
	pdbBlockedAfter     = 30 * time.Minute // a budget allowing no disruption for this long blocks drains
)

// finalizerOwners names the controller responsible for well-known
// finalizers, so the message says who must act.
var finalizerOwners = map[string]string{
	"kubernetes":                   "the namespace controller in kube-controller-manager, which waits until everything inside the namespace is gone",
	"kubernetes.io/pvc-protection": "the pvc-protection controller in kube-controller-manager, which waits until no pod uses the claim",
	"kubernetes.io/pv-protection":  "the pv-protection controller in kube-controller-manager, which waits until the volume is unbound",
	"foregroundDeletion":           "the garbage collector, which waits for dependents to be deleted first",
	"orphan":                       "the garbage collector, which orphans dependents first",
}

func finalizerOwner(f string) string {
	if o, ok := finalizerOwners[f]; ok {
		return o
	}
	if strings.HasPrefix(f, "external-attacher/") {
		return "the CSI external-attacher for that driver"
	}
	return "the controller that added it; if that controller was uninstalled, nothing will ever remove it"
}

func describeFinalizers(fs []string) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, fmt.Sprintf("- `%s`: %s", f, finalizerOwner(f)))
	}
	return out
}

// CheckStuckFinalizers reports allowlisted namespaces and claims whose
// deletion is held by finalizers (PLAN-002 10.2).
func CheckStuckFinalizers(ctx context.Context, deps *Deps) {
	now := deps.clock()
	nss, err := deps.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "namespaces", "")
	} else {
		for i := range nss.Items {
			n := &nss.Items[i]
			if deps.Policy().Watched(n.Name) {
				checkNamespaceStuck(ctx, deps, n, now)
			}
		}
	}
	for _, ns := range watchedNamespaces(ctx, deps) {
		pvcs, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "persistentvolumeclaims", ns)
			continue
		}
		for i := range pvcs.Items {
			checkPVCStuck(ctx, deps, &pvcs.Items[i], now)
		}
	}
}

func checkNamespaceStuck(ctx context.Context, deps *Deps, n *corev1.Namespace, now time.Time) {
	if n.DeletionTimestamp == nil || now.Sub(n.DeletionTimestamp.Time) < finalizerStuckAfter {
		return
	}
	var details []string
	for _, c := range n.Status.Conditions {
		if c.Status == corev1.ConditionTrue {
			details = append(details, fmt.Sprintf("%s: %s", c.Type, c.Message))
		}
	}
	fs := make([]string, 0, len(n.Spec.Finalizers))
	for _, f := range n.Spec.Finalizers {
		fs = append(fs, string(f))
	}
	details = append(details, describeFinalizers(append(fs, n.Finalizers...))...)
	report(ctx, deps, finding{
		Reason: "NamespaceStuckTerminating", Namespace: n.Name, Workload: "namespace/" + n.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: n.Name,
		Summary: fmt.Sprintf("namespace has been terminating for %s", now.Sub(n.DeletionTimestamp.Time).Round(time.Minute)),
		Details: details,
		Fix:     "the conditions above name what is left; delete or unblock those objects first, and if a discovery failure is listed, fix or remove the broken APIService. Removing a finalizer by hand skips that controller's cleanup",
	})
}

func checkPVCStuck(ctx context.Context, deps *Deps, pvc *corev1.PersistentVolumeClaim, now time.Time) {
	if pvc.DeletionTimestamp == nil || len(pvc.Finalizers) == 0 || now.Sub(pvc.DeletionTimestamp.Time) < finalizerStuckAfter {
		return
	}
	details := describeFinalizers(pvc.Finalizers)
	if users := podsUsingClaim(ctx, deps, pvc.Namespace, pvc.Name); len(users) > 0 {
		details = append(details, fmt.Sprintf("Still mounted by: `%s`", strings.Join(users, "`, `")))
	}
	report(ctx, deps, finding{
		Reason: "PVCStuckTerminating", Namespace: pvc.Namespace, Workload: "pvc/" + pvc.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: pvc.Name,
		Summary: fmt.Sprintf("claim has been terminating for %s", now.Sub(pvc.DeletionTimestamp.Time).Round(time.Minute)),
		Details: details,
		Fix:     "stop or delete the pods that still use the claim; the protection finalizer is then removed on its own",
	})
}

func podsUsingClaim(ctx context.Context, deps *Deps, ns, claim string) []string {
	pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "pods", ns)
		return nil
	}
	var out []string
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				out = append(out, p.Name)
				break
			}
		}
	}
	return out
}

// CheckDisruptionBudgets reports budgets that have allowed no disruption
// for long, which makes node drains, upgrades and our own node-pressure
// evictions hang (PLAN-002 10.6).
func CheckDisruptionBudgets(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		pdbs, err := deps.Client.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "poddisruptionbudgets", ns)
			continue
		}
		for i := range pdbs.Items {
			checkPDB(ctx, deps, &pdbs.Items[i], now)
		}
	}
}

func checkPDB(ctx context.Context, deps *Deps, pdb *policyv1.PodDisruptionBudget, now time.Time) {
	st := pdb.Status
	if st.ExpectedPods == 0 || st.DisruptionsAllowed > 0 {
		return
	}
	since := pdb.CreationTimestamp.Time
	if c := metaCondition(st.Conditions, policyv1.DisruptionAllowedCondition); c != nil {
		since = c.LastTransitionTime.Time
	}
	if now.Sub(since) < pdbBlockedAfter {
		return
	}
	fix := "the budget allows no disruption even with every pod healthy, so drains and upgrades hang; use maxUnavailable: 1, or add a replica above minAvailable"
	if st.CurrentHealthy < st.DesiredHealthy {
		fix = "pods covered by the budget are unhealthy, so no disruption is allowed; fix those pods first"
	}
	report(ctx, deps, finding{
		Reason: "PDBBlocksDisruption", Namespace: pdb.Namespace, Workload: "pdb/" + pdb.Name,
		Severity: eventsvc.SevWarning, Rung: RungGuided, Subject: pdb.Name,
		Summary: fmt.Sprintf("disruption budget has allowed no eviction for %s", now.Sub(since).Round(time.Minute)),
		Details: []string{fmt.Sprintf("Healthy %d of %d expected, %d required.", st.CurrentHealthy, st.ExpectedPods, st.DesiredHealthy)},
		Fix:     fix,
	})
}

func metaCondition(cs []metav1.Condition, t string) *metav1.Condition {
	for i := range cs {
		if cs[i].Type == t {
			return &cs[i]
		}
	}
	return nil
}

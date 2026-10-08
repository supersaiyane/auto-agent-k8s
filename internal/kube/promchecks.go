package kube

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// Thresholds for the Prometheus based checks (PLAN-002 10.9, 10.10, 10.13).
const (
	throttleRatioMin  = 0.25 // a quarter of CPU periods throttled
	volumeWarnRatio   = 0.85
	volumeCritRatio   = 0.95
	etcdLeaderChanges = 3   // per hour
	etcdQuotaRatio    = 0.8 // database size against its backend quota
	growthFactor      = 1.5 // proposed limit or size, against the current one
)

// queryVector runs a PromQL query for a detector. No Prometheus means
// nothing to check; any other failure is counted and logged, never alerted.
func queryVector(ctx context.Context, deps *Deps, check, q string) ([]metrics.Sample, bool) {
	if deps.Metrics == nil {
		return nil, false
	}
	v, err := deps.Metrics.QueryVector(ctx, q)
	if errors.Is(err, metrics.ErrNoPromQL) {
		return nil, false
	}
	if err != nil {
		obs.HandlerErrorsTotal.WithLabelValues(check, "prometheus").Inc()
		klog.Warningf("%s: prometheus query failed: %v", check, err)
		return nil, false
	}
	return v, true
}

// watchMatcher is a PromQL regex of the watched namespaces, so the query
// itself never reads outside the watch scope (constraint 4).
func watchMatcher(ctx context.Context, deps *Deps) string {
	watched := watchedNamespaces(ctx, deps)
	names := make([]string, 0, len(watched))
	for _, ns := range watched {
		names = append(names, metricsLabel(ns))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// metricsLabel keeps only characters a namespace name can have.
func metricsLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

// CheckResourcePressure reports throttled containers (10.9) and claims
// close to full (10.10). Without Prometheus it does nothing.
func CheckResourcePressure(ctx context.Context, deps *Deps) {
	nsRe := watchMatcher(ctx, deps)
	if nsRe == "" {
		return
	}
	checkCPUThrottling(ctx, deps, nsRe)
	checkVolumeFullness(ctx, deps, nsRe)
}

func checkCPUThrottling(ctx context.Context, deps *Deps, nsRe string) {
	q := fmt.Sprintf(`sum by (namespace, pod, container) (rate(container_cpu_cfs_throttled_periods_total{namespace=~"%[1]s",container!=""}[5m]))`+
		` / sum by (namespace, pod, container) (rate(container_cpu_cfs_periods_total{namespace=~"%[1]s",container!=""}[5m]))`, nsRe)
	v, ok := queryVector(ctx, deps, "CPUThrottling", q)
	if !ok {
		return
	}
	pods := objCache[corev1.Pod]{list: func(ns string) ([]corev1.Pod, error) {
		l, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}, name: func(p *corev1.Pod) string { return p.Name }, resource: "pods"}
	for _, s := range v {
		ns, podName, cname := s.Labels["namespace"], s.Labels["pod"], s.Labels["container"]
		if s.Value < throttleRatioMin || !deps.Policy().Watched(ns) {
			continue
		}
		p := pods.get(ns, podName)
		if p == nil {
			continue // gone since the sample was taken
		}
		c := containerSpec(p, cname)
		if c == nil {
			continue
		}
		limit := c.Resources.Limits.Cpu()
		if limit.IsZero() {
			continue // throttling needs a limit; nothing to propose
		}
		proposed := resource.NewMilliQuantity(roundUp(int64(float64(limit.MilliValue())*growthFactor), 50), resource.DecimalSI)
		report(ctx, deps, finding{
			Reason: "CPUThrottled", Namespace: ns, Workload: ownerName(p), Pod: podName, Node: p.Spec.NodeName,
			Severity: eventsvc.SevWarning, Rung: RungGuided, Target: RungApprove, Subject: podName + "/" + cname,
			Summary: fmt.Sprintf("container `%s` is throttled in %.0f percent of CPU periods", cname, s.Value*100),
			Details: []string{fmt.Sprintf("CPU limit %s, request %s.", limit.String(), c.Resources.Requests.Cpu().String())},
			Fix: fmt.Sprintf("raise the CPU limit of `%s` from %s to %s, or remove the CPU limit and keep the request; throttling adds latency even when the node has idle CPU",
				cname, limit.String(), proposed.String()),
		})
	}
}

func checkVolumeFullness(ctx context.Context, deps *Deps, nsRe string) {
	q := fmt.Sprintf(`max by (namespace, persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace=~"%[1]s"})`+
		` / max by (namespace, persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes{namespace=~"%[1]s"})`, nsRe)
	v, ok := queryVector(ctx, deps, "VolumeAlmostFull", q)
	if !ok {
		return
	}
	claims := objCache[corev1.PersistentVolumeClaim]{list: func(ns string) ([]corev1.PersistentVolumeClaim, error) {
		l, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}, name: func(c *corev1.PersistentVolumeClaim) string { return c.Name }, resource: "persistentvolumeclaims"}
	for _, s := range v {
		ns, claim := s.Labels["namespace"], s.Labels["persistentvolumeclaim"]
		if s.Value < volumeWarnRatio || !deps.Policy().Watched(ns) {
			continue
		}
		pvc := claims.get(ns, claim)
		if pvc == nil {
			continue
		}
		sev := eventsvc.SevWarning
		if s.Value >= volumeCritRatio {
			sev = eventsvc.SevCritical
		}
		f := finding{
			Reason: "VolumeAlmostFull", Namespace: ns, Workload: "pvc/" + claim, Severity: sev, Rung: RungGuided, Subject: claim,
			Summary: fmt.Sprintf("claim is %.0f percent full", s.Value*100),
		}
		capacity := pvc.Status.Capacity[corev1.ResourceStorage]
		expandable, class := volumeExpandable(ctx, deps, pvc)
		if expandable && !capacity.IsZero() {
			gi := int64(math.Ceil(float64(capacity.Value()) * growthFactor / (1 << 30)))
			f.Target = RungApprove
			f.Details = []string{fmt.Sprintf("Capacity %s; StorageClass `%s` allows expansion.", capacity.String(), class)}
			f.Fix = fmt.Sprintf("expand the claim to %dGi: `kubectl patch pvc -n %s %s -p '{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"%dGi\"}}}}'`", gi, ns, claim, gi)
		} else {
			f.Details = []string{fmt.Sprintf("Capacity %s; StorageClass `%s` does not allow expansion.", capacity.String(), class)}
			f.Fix = "free space or move the data to a larger volume; this class cannot be expanded in place"
		}
		report(ctx, deps, f)
	}
}

// volumeExpandable reports whether the claim's StorageClass allows
// expansion; an unreadable class counts as "no", so nothing is offered.
func volumeExpandable(ctx context.Context, deps *Deps, pvc *corev1.PersistentVolumeClaim) (bool, string) {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false, "none"
	}
	name := *pvc.Spec.StorageClassName
	sc, err := deps.Client.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			countAPIError(err, "storageclasses", "")
		}
		return false, name
	}
	return sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion, name
}

// CheckControlPlane reports etcd health (10.13) and deprecated API use
// (10.14) from control plane metrics. Managed clusters usually do not
// expose these; an empty result is silence, not an alert.
func CheckControlPlane(ctx context.Context, deps *Deps) {
	checkEtcd(ctx, deps)
	checkDeprecatedAPIs(ctx, deps)
}

// etcdChecks are the etcd queries, each with the test that makes a
// sample a finding and the words for it.
var etcdChecks = []struct {
	reason, query string
	bad           func(float64) bool
	sev           eventsvc.Severity
	summary, fix  string
}{
	{"EtcdNoLeader", `max by (instance) (etcd_server_has_leader)`, func(v float64) bool { return v == 0 }, eventsvc.SevCritical,
		"etcd member has no leader; the API server cannot write", "check etcd member health and the network between control plane nodes"},
	{"EtcdLeaderChurn", `sum by (instance) (increase(etcd_server_leader_changes_seen_total[1h]))`, func(v float64) bool { return v > etcdLeaderChanges }, eventsvc.SevWarning,
		"etcd changed leader %.0f times in the last hour", "usually slow disks or a congested network between control plane nodes; check etcd disk fsync latency"},
	{"EtcdDBNearQuota", `max by (instance) (etcd_mvcc_db_total_size_in_bytes / etcd_server_quota_backend_bytes)`, func(v float64) bool { return v >= etcdQuotaRatio }, eventsvc.SevWarning,
		"etcd database is at %.0f percent of its quota", "compact and defragment etcd; at the quota etcd raises an alarm and refuses writes"},
}

func checkEtcd(ctx context.Context, deps *Deps) {
	for _, c := range etcdChecks {
		v, ok := queryVector(ctx, deps, c.reason, c.query)
		if !ok {
			continue
		}
		for _, s := range v {
			if !c.bad(s.Value) {
				continue
			}
			value := s.Value
			if c.reason == "EtcdDBNearQuota" {
				value *= 100
			}
			summary := c.summary
			if strings.Contains(summary, "%") {
				summary = fmt.Sprintf(summary, value)
			}
			report(ctx, deps, finding{
				Reason: c.reason, Workload: "etcd/" + s.Labels["instance"], Severity: c.sev, Rung: RungAlert,
				Subject: s.Labels["instance"], Summary: summary, Fix: c.fix,
			})
		}
	}
}

// apiReplacements names the stable API for well-known deprecated ones,
// keyed by resource and group.
var apiReplacements = map[string]string{
	"cronjobs.batch":                                               "batch/v1",
	"horizontalpodautoscalers.autoscaling":                         "autoscaling/v2",
	"ingresses.networking.k8s.io":                                  "networking.k8s.io/v1",
	"ingresses.extensions":                                         "networking.k8s.io/v1",
	"poddisruptionbudgets.policy":                                  "policy/v1",
	"podsecuritypolicies.policy":                                   "nothing: use Pod Security Admission",
	"endpointslices.discovery.k8s.io":                              "discovery.k8s.io/v1",
	"events.events.k8s.io":                                         "events.k8s.io/v1",
	"runtimeclasses.node.k8s.io":                                   "node.k8s.io/v1",
	"csistoragecapacities.storage.k8s.io":                          "storage.k8s.io/v1",
	"flowschemas.flowcontrol.apiserver.k8s.io":                     "flowcontrol.apiserver.k8s.io/v1",
	"prioritylevelconfigurations.flowcontrol.apiserver.k8s.io":     "flowcontrol.apiserver.k8s.io/v1",
	"customresourcedefinitions.apiextensions.k8s.io":               "apiextensions.k8s.io/v1",
	"mutatingwebhookconfigurations.admissionregistration.k8s.io":   "admissionregistration.k8s.io/v1",
	"validatingwebhookconfigurations.admissionregistration.k8s.io": "admissionregistration.k8s.io/v1",
	"componentstatuses.":                                           "nothing: use the API server /readyz and /livez endpoints",
}

func checkDeprecatedAPIs(ctx context.Context, deps *Deps) {
	v, ok := queryVector(ctx, deps, "DeprecatedAPIInUse",
		`max by (group, version, resource, removed_release) (apiserver_requested_deprecated_apis)`)
	if !ok {
		return
	}
	for _, s := range v {
		group, version, res, removed := s.Labels["group"], s.Labels["version"], s.Labels["resource"], s.Labels["removed_release"]
		gv := version
		if group != "" {
			gv = group + "/" + version
		}
		replacement, known := apiReplacements[res+"."+group]
		if !known {
			replacement = "the stable version listed in the Kubernetes deprecation guide"
		}
		f := finding{
			Reason: "DeprecatedAPIInUse", Workload: "api/" + gv + "/" + res, Rung: RungGuided, Subject: gv + "/" + res,
			Severity: eventsvc.SevInfo,
			Summary:  fmt.Sprintf("something requested deprecated `%s` `%s`", gv, res),
			Fix:      fmt.Sprintf("move manifests, charts and clients to %s; the API server audit log names the caller", replacement),
			Details:  []string{"Deprecated with no removal release announced yet."},
		}
		if removed != "" {
			f.Severity = eventsvc.SevWarning
			f.Details = []string{fmt.Sprintf("Removed in Kubernetes %s: upgrading to it breaks whatever still calls this API.", removed)}
		}
		report(ctx, deps, f)
	}
}

// objCache lists each namespace's objects once per pass.
type objCache[T any] struct {
	list     func(ns string) ([]T, error)
	name     func(*T) string
	resource string
	byNS     map[string]map[string]*T
}

func (c *objCache[T]) get(ns, name string) *T {
	if c.byNS == nil {
		c.byNS = map[string]map[string]*T{}
	}
	if _, ok := c.byNS[ns]; !ok {
		c.byNS[ns] = map[string]*T{}
		items, err := c.list(ns)
		if err != nil {
			countAPIError(err, c.resource, ns)
		}
		for i := range items {
			c.byNS[ns][c.name(&items[i])] = &items[i]
		}
	}
	return c.byNS[ns][name]
}

func roundUp(v, step int64) int64 {
	return (v + step - 1) / step * step
}

package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// promAnswers answers QueryVector by the first key found in the query.
func promAnswers(answers map[string][]metrics.Sample) func(string) ([]metrics.Sample, error) {
	return func(q string) ([]metrics.Sample, error) {
		for key, v := range answers {
			if strings.Contains(q, key) {
				return v, nil
			}
		}
		return nil, nil // a metric the cluster does not expose: empty, not an error
	}
}

func sample(v float64, kv ...string) metrics.Sample {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return metrics.Sample{Labels: l, Value: v}
}

func promHarness(t *testing.T, answers map[string][]metrics.Sample, objs ...runtime.Object) *findingHarness {
	h := newFindingHarness(t, objs...)
	h.deps.Metrics = &mockMetrics{vector: promAnswers(answers)}
	return h
}

// 10.9: no alert and no error without Prometheus; a failed query is
// counted, not alerted.
func TestPromChecks_SilentWithoutPrometheus(t *testing.T) {
	h := newFindingHarness(t)
	h.deps.Metrics = &mockMetrics{}
	errs := testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("CPUThrottling", "prometheus"))
	CheckResourcePressure(context.Background(), h.deps)
	CheckControlPlane(context.Background(), h.deps)
	h.deps.Metrics = nil
	CheckResourcePressure(context.Background(), h.deps)
	if len(h.slack.Messages()) != 0 || testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("CPUThrottling", "prometheus")) != errs {
		t.Fatal("no Prometheus means nothing to say and nothing to count")
	}

	h.deps.Metrics = &mockMetrics{vector: func(string) ([]metrics.Sample, error) { return nil, errors.New("503") }}
	CheckResourcePressure(context.Background(), h.deps)
	if len(h.slack.Messages()) != 0 || testutil.ToFloat64(obs.HandlerErrorsTotal.WithLabelValues("CPUThrottling", "prometheus"))-errs != 1 {
		t.Fatal("a failed query is counted, never alerted")
	}
}

// 10.9
func TestCPUThrottling(t *testing.T) {
	limited := func(name, limit string) *corev1.Pod {
		return pod(name, func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}}
			if limit != "" {
				p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)}
			}
		})
	}
	h := promHarness(t, map[string][]metrics.Sample{"container_cpu_cfs_throttled_periods_total": {
		sample(0.4, "namespace", "default", "pod", "api", "container", "app"),
		sample(0.1, "namespace", "default", "pod", "calm", "container", "app"),
		sample(0.9, "namespace", "payments", "pod", "api", "container", "app"),
		sample(0.9, "namespace", "default", "pod", "unlimited", "container", "app"),
		sample(0.9, "namespace", "default", "pod", "gone", "container", "app"),
		sample(0.9, "namespace", "default", "pod", "api", "container", "sidecar"),
	}}, limited("api", "500m"), limited("calm", "500m"), limited("unlimited", ""))
	CheckResourcePressure(context.Background(), h.deps)
	msg := h.expect(t, "CPUThrottled", RungGuided, 1)[0]
	for _, want := range []string{"container `app` is throttled in 40 percent", "CPU limit 500m, request 200m",
		"from 500m to 750m", "R3 approve to fix arrives"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if q := allowlistMatcher(h.deps); q != "default" {
		t.Fatalf("the query is limited to the allowlist: %q", q)
	}
	if metricsLabel(`a"b|c`) != "a_b_c" || roundUp(751, 50) != 800 {
		t.Fatal("helpers")
	}
}

// 10.10: expansion is offered only when the StorageClass allows it.
func TestVolumeAlmostFull(t *testing.T) {
	yes, no := true, false
	claim := func(name, class string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
			Status:     corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		}
	}
	h := promHarness(t, map[string][]metrics.Sample{"kubelet_volume_stats_used_bytes": {
		sample(0.90, "namespace", "default", "persistentvolumeclaim", "data"),
		sample(0.97, "namespace", "default", "persistentvolumeclaim", "logs"),
		sample(0.50, "namespace", "default", "persistentvolumeclaim", "roomy"),
		sample(0.90, "namespace", "default", "persistentvolumeclaim", "orphan"),
		sample(0.99, "namespace", "payments", "persistentvolumeclaim", "data"),
	}},
		claim("data", "expandable"), claim("logs", "fixed"), claim("roomy", "expandable"), claim("orphan", "missing"),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "expandable"}, AllowVolumeExpansion: &yes},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fixed"}, AllowVolumeExpansion: &no})
	CheckResourcePressure(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "VolumeAlmostFull", RungGuided, 3), "\n")
	for _, want := range []string{"`default/pvc/data`: claim is 90 percent full", "expand the claim to 15Gi", `"storage":"15Gi"`,
		"`default/pvc/logs`: claim is 97 percent full", "`fixed` does not allow expansion", "`missing` does not allow expansion"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	if strings.Count(msgs, "expand the claim") != 1 || strings.Contains(msgs, "roomy") || strings.Contains(msgs, "99 percent") {
		t.Fatalf("expansion offered only where allowed; quiet below the threshold and outside the allowlist:\n%s", msgs)
	}
	var sev []string
	for _, e := range h.rec.Recent(10) {
		sev = append(sev, e.Workload+"="+string(e.Severity))
	}
	if !strings.Contains(strings.Join(sev, " "), "pvc/logs=critical") || !strings.Contains(strings.Join(sev, " "), "pvc/data=warning") {
		t.Fatalf("severity follows fullness: %v", sev)
	}
	if ok, name := volumeExpandable(context.Background(), h.deps, &corev1.PersistentVolumeClaim{}); ok || name != "none" {
		t.Fatal("a claim without a class cannot be expanded")
	}
}

// 10.13: no noise when the metrics do not exist (a managed cluster).
func TestEtcdHealth(t *testing.T) {
	h := promHarness(t, nil)
	CheckControlPlane(context.Background(), h.deps)
	if len(h.slack.Messages()) != 0 {
		t.Fatal("no etcd metrics, no findings")
	}

	h = promHarness(t, map[string][]metrics.Sample{
		"etcd_server_has_leader":                {sample(0, "instance", "10.0.0.1:2379"), sample(1, "instance", "10.0.0.2:2379")},
		"etcd_server_leader_changes_seen_total": {sample(5, "instance", "10.0.0.1:2379"), sample(1, "instance", "10.0.0.2:2379")},
		"etcd_mvcc_db_total_size_in_bytes":      {sample(0.85, "instance", "10.0.0.1:2379"), sample(0.5, "instance", "10.0.0.2:2379")},
	})
	CheckControlPlane(context.Background(), h.deps)
	if m := h.expect(t, "EtcdNoLeader", RungAlert, 1)[0]; !strings.HasPrefix(m, "*EtcdNoLeader* `etcd/10.0.0.1:2379`: etcd member has no leader") {
		t.Errorf("no leader:\n%s", m)
	}
	if m := h.expect(t, "EtcdLeaderChurn", RungAlert, 1)[0]; !strings.Contains(m, "5 times in the last hour") {
		t.Errorf("churn:\n%s", m)
	}
	if m := h.expect(t, "EtcdDBNearQuota", RungAlert, 1)[0]; !strings.Contains(m, "85 percent of its quota") {
		t.Errorf("quota:\n%s", m)
	}
}

// 10.14: one removed and one deprecated API, named with the replacement.
func TestDeprecatedAPIs(t *testing.T) {
	h := promHarness(t, map[string][]metrics.Sample{"apiserver_requested_deprecated_apis": {
		sample(1, "group", "batch", "version", "v1beta1", "resource", "cronjobs", "removed_release", "1.25"),
		sample(1, "group", "flowcontrol.apiserver.k8s.io", "version", "v1beta3", "resource", "flowschemas"),
		sample(1, "group", "example.com", "version", "v1alpha1", "resource", "widgets"),
		sample(1, "group", "", "version", "v1", "resource", "componentstatuses"),
	}})
	CheckControlPlane(context.Background(), h.deps)
	msgs := strings.Join(h.expect(t, "DeprecatedAPIInUse", RungGuided, 4), "\n")
	for _, want := range []string{
		"deprecated `batch/v1beta1` `cronjobs`", "Removed in Kubernetes 1.25", "move manifests, charts and clients to batch/v1",
		"`flowcontrol.apiserver.k8s.io/v1beta3` `flowschemas`", "no removal release announced", "to flowcontrol.apiserver.k8s.io/v1",
		"the stable version listed in the Kubernetes deprecation guide", "deprecated `v1` `componentstatuses`", "/readyz and /livez",
	} {
		if !strings.Contains(msgs, want) {
			t.Errorf("messages lack %q:\n%s", want, msgs)
		}
	}
	for _, e := range h.rec.Recent(10) {
		removed := strings.Contains(e.Workload, "cronjobs")
		if removed != (e.Severity == "warning") {
			t.Errorf("%s: a scheduled removal is a warning, the rest info (%s)", e.Workload, e.Severity)
		}
	}
}

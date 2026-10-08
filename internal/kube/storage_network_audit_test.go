package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

func claim(name string, phase corev1.PersistentVolumeClaimPhase, class *string, age time.Duration) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.NewTime(testNow.Add(-age))},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: class, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase}}
}

func str(s string) *string { return &s }

// Phase 12 audit of CheckStorageIssues.
//
// Claims: claims whose volume is gone, and pending claims whose StorageClass
// cannot provision them.
// Bugs and misses found: an empty class name (static binding) was looked up
// as a class and counted as an API error every pass; a claim with no class
// in a cluster without a default class was never reported (now
// NoDefaultStorageClass); one class read per claim per pass (now one list).
// Rung R1: storage changes risk data.
func TestAudit_StorageIssues(t *testing.T) {
	std := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}}
	objs := []runtime.Object{std,
		claim("lost", corev1.ClaimLost, str("standard"), time.Hour),
		claim("typo", corev1.ClaimPending, str("standrad"), time.Hour),
		claim("static", corev1.ClaimPending, str(""), time.Hour),
		claim("ok", corev1.ClaimPending, str("standard"), time.Hour),
		claim("noclass", corev1.ClaimPending, nil, time.Hour),
	}
	h := newFindingHarness(t, objs...)
	CheckStorageIssues(context.Background(), h.deps)
	h.expect(t, "PVCLost", RungGuided, 1)
	if m := h.expect(t, "StorageClassNotFound", RungGuided, 1)[0]; !strings.Contains(m, "pvc/typo") || !strings.Contains(m, "`standrad`") {
		t.Fatalf("message: %s", m)
	}
	h.expect(t, "NoDefaultStorageClass", RungGuided, 1)

	def := std.DeepCopy()
	def.Annotations = map[string]string{defaultClassAnnotation: "true"}
	h2 := newFindingHarness(t, def, claim("noclass", corev1.ClaimPending, nil, time.Hour))
	CheckStorageIssues(context.Background(), h2.deps)
	h2.quiet(t)

	expectCounted(t, "storageclasses", func(d *Deps) { CheckStorageIssues(context.Background(), d) })
	expectCounted(t, "persistentvolumeclaims", func(d *Deps) { CheckStorageIssues(context.Background(), d) })
}

// Phase 12 audit of CheckPendingPVCs.
//
// Claims: a claim stuck in Pending, with the provisioner's latest event.
// Bugs found: (1) a claim waiting for its first consumer (WaitForFirstConsumer
// binding) was reported though that is normal; (2) "latest event" was the
// first item of an unordered list; (3) a failed list was dropped; (4) the
// age used the real clock. Rung R1.
func TestAudit_PendingPVCs(t *testing.T) {
	ev := func(name, obj, reason, msg string, at time.Duration) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Type: "Warning", Reason: reason, Message: msg,
			InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: obj}, LastTimestamp: metav1.NewTime(testNow.Add(-at))}
	}
	h := newFindingHarness(t,
		claim("stuck", corev1.ClaimPending, str("standard"), time.Hour),
		ev("old", "stuck", "ExternalProvisioning", "waiting for a volume", 50*time.Minute),
		ev("new", "stuck", "ProvisioningFailed", "quota exceeded", time.Minute),
		claim("waits", corev1.ClaimPending, str("local"), time.Hour),
		ev("w", "waits", "WaitForFirstConsumer", "waiting for first consumer to be created before binding", time.Minute),
		claim("young", corev1.ClaimPending, str("standard"), time.Minute),
		claim("bound", corev1.ClaimBound, str("standard"), time.Hour))
	CheckPendingPVCs(context.Background(), h.deps)
	m := h.expect(t, "PVCPending", RungGuided, 1)[0]
	for _, want := range []string{"pvc/stuck", "Pending for 1h0m0s", "Latest event: Warning ProvisioningFailed: quota exceeded", "size 10Gi"} {
		if !strings.Contains(m, want) {
			t.Errorf("message lacks %q:\n%s", want, m)
		}
	}
	if newestEvent(nil, "x") != nil {
		t.Fatal("no events, no newest")
	}
	expectCounted(t, "persistentvolumeclaims", func(d *Deps) { CheckPendingPVCs(context.Background(), d) })
}

// Phase 12 audit of CheckNetworkIssues.
//
// Claims: CoreDNS down or degraded, LoadBalancers without an address, and
// Ingress backends that cannot serve.
// Bugs found: (1) an Ingress with a resource backend (not a Service)
// dereferenced a nil Service and crashed the check; (2) the default backend
// was never checked; (3) backends were read from the deprecated Endpoints
// API, one call per path (ISS-035); (4) CoreDNS pods in kube-system were read
// although kube-system is outside the default watch scope (constraint 4), and
// the fallback read dropped its error; (5) a pod counted as ready if any one
// container was; (6) the LoadBalancer age used the real clock.
// Rungs: DNS and Ingress R1; LoadBalancer R0 (the cause is outside the cluster).
func TestAudit_NetworkIssues(t *testing.T) {
	old := metav1.NewTime(testNow.Add(-time.Hour))
	yes := true
	path := func(p, svc string) networkingv1.HTTPIngressPath {
		pt := networkingv1.PathTypePrefix
		return networkingv1.HTTPIngressPath{Path: p, PathType: &pt, Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: svc}}}
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "default"}, Spec: networkingv1.IngressSpec{
		DefaultBackend: &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "fallback"}},
		Rules: []networkingv1.IngressRule{{Host: "shop.example.test", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{
			path("/", "web"), path("/api", "api"),
			{Path: "/static", Backend: networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}}},
		}}}}}}}
	svc := func(name string, typ corev1.ServiceType) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: old}, Spec: corev1.ServiceSpec{Type: typ}}
	}
	lbDone := svc("lb-ok", corev1.ServiceTypeLoadBalancer)
	lbDone.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.7"}}
	objs := []runtime.Object{ing, svc("web", corev1.ServiceTypeClusterIP), svc("fallback", corev1.ServiceTypeClusterIP),
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
			Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &yes}}}},
		svc("lb", corev1.ServiceTypeLoadBalancer), lbDone}
	h := newFindingHarness(t, objs...)
	CheckNetworkIssues(context.Background(), h.deps)
	if m := h.expect(t, "IngressBackendMissing", RungGuided, 1)[0]; !strings.Contains(m, "Service `api`") || !strings.Contains(m, `host "shop.example.test" path "/api"`) {
		t.Fatalf("missing backend: %s", m)
	}
	if m := h.expect(t, "IngressNoBackends", RungGuided, 1)[0]; !strings.Contains(m, "Service `fallback`") || !strings.Contains(m, "default backend") {
		t.Fatalf("default backend: %s", m)
	}
	if m := h.expect(t, "LoadBalancerPending", RungAlert, 1)[0]; !strings.Contains(m, "service/lb`") {
		t.Fatalf("load balancer: %s", m)
	}

	// CoreDNS: not read unless kube-system is watched; then down or degraded.
	dns := func(name string, ready bool) *corev1.Pod {
		p := pod(name, func(p *corev1.Pod) { p.Namespace = "kube-system"; p.Labels = map[string]string{"k8s-app": "kube-dns"} })
		readyFor(p, ready, time.Hour)
		return p
	}
	quiet := newFindingHarness(t, dns("coredns-a", false))
	checkDNSHealth(context.Background(), quiet.deps)
	quiet.quiet(t)
	watchSystem := func(h *findingHarness) {
		p := *h.deps.Policy()
		p.WatchNamespaces = policy.NamespaceSet("default", "kube-system")
		h.deps.Policies = policy.Static(&p)
	}
	down := newFindingHarness(t, dns("coredns-a", false), dns("coredns-b", false))
	watchSystem(down)
	checkDNSHealth(context.Background(), down.deps)
	down.expect(t, "DNSDown", RungGuided, 1)
	part := newFindingHarness(t, dns("coredns-a", true), dns("coredns-b", false))
	watchSystem(part)
	checkDNSHealth(context.Background(), part.deps)
	part.expect(t, "DNSDegraded", RungGuided, 1)

	expectCounted(t, "ingresses", func(d *Deps) { checkIngressBackends(context.Background(), d) })
}

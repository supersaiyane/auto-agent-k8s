package kube

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	"github.com/supersaiyane/auto-agent-k8s/internal/metrics"
	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// probeHarness runs the node probes with a fake resolver, dialer and clock.
type probeHarness struct {
	*findingHarness
	p       *netProber
	resolve map[string]error         // host -> error (nil: answers)
	slow    map[string]time.Duration // host -> how long the answer takes
	dialErr map[string]error         // addr -> error
	now     time.Time
}

func newProbeHarness(t *testing.T, cfg config.Probes, objs ...runtime.Object) *probeHarness {
	t.Helper()
	h := &probeHarness{findingHarness: newFindingHarness(t, objs...), resolve: map[string]error{}, slow: map[string]time.Duration{},
		dialErr: map[string]error{}, now: testNow}
	h.deps.NodeName = "node-a"
	h.deps.Now = func() time.Time { return h.now }
	h.p = newNetProber(h.deps, cfg)
	h.p.resolve = func(_ context.Context, host string) ([]string, error) {
		h.now = h.now.Add(h.slow[host])
		return []string{"10.96.0.1"}, h.resolve[host]
	}
	h.p.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		if err := h.dialErr[addr]; err != nil {
			return nil, err
		}
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	}
	return h
}

// ISS-033: a node that cannot resolve the API server's Service name, twice
// in a row, is reported; one blip is not; slow answers are; an external
// name failing is upstream DNS (R0).
func TestNetProbe_DNS(t *testing.T) {
	cfg := config.Probes{DNS: true, ClusterDomain: "cluster.local", ExternalName: "example.test"}
	h := newProbeHarness(t, cfg)
	internal := "kubernetes.default.svc.cluster.local"
	h.resolve[internal] = errors.New("i/o timeout")
	h.p.probeOnce(context.Background())
	h.quiet(t) // one failure is a blip
	h.p.probeOnce(context.Background())
	if m := h.expect(t, "DNSResolutionFailed", RungGuided, 1)[0]; !strings.Contains(m, "node/node-a") || !strings.Contains(m, internal) {
		t.Fatalf("internal: %s", m)
	}
	h.p.probeOnce(context.Background())
	h.expect(t, "DNSResolutionFailed", RungGuided, 1) // reported once per run of failures

	up := newProbeHarness(t, cfg)
	up.resolve["example.test"] = errors.New("no such host")
	up.p.probeOnce(context.Background())
	up.p.probeOnce(context.Background())
	up.expect(t, "DNSResolutionFailed", RungAlert, 1)

	slow := newProbeHarness(t, cfg)
	slow.slow[internal] = 3 * time.Second
	slow.p.probeOnce(context.Background())
	slow.p.probeOnce(context.Background())
	if m := slow.expect(t, "DNSSlow", RungGuided, 1)[0]; !strings.Contains(m, "took 3s") {
		t.Fatalf("slow: %s", m)
	}

	recover := newProbeHarness(t, cfg)
	recover.resolve[internal] = errors.New("x")
	recover.p.probeOnce(context.Background())
	recover.resolve[internal] = nil
	recover.p.probeOnce(context.Background())
	recover.resolve[internal] = errors.New("x")
	recover.p.probeOnce(context.Background())
	recover.quiet(t) // a success resets the count
}

// ISS-036: a Service with ready endpoints that this node cannot dial is
// reported (kube-proxy or the CNI here); one without endpoints is not dialled.
func TestNetProbe_ServicesAndEgress(t *testing.T) {
	yes := true
	svc := func(name, ip string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1.ServiceSpec{ClusterIP: ip, Ports: []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}}}}
	}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "api-x", Namespace: "default", Labels: map[string]string{discoveryv1.LabelServiceName: "api"}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.5"}, Conditions: discoveryv1.EndpointConditions{Ready: &yes}}}}
	h := newProbeHarness(t, config.Probes{Services: true, EgressTarget: "203.0.113.9:443"},
		svc("api", "10.96.0.20"), svc("empty", "10.96.0.21"), svc("headless", corev1.ClusterIPNone), slice)
	h.dialErr["10.96.0.20:80"] = errors.New("connection refused")
	h.dialErr["203.0.113.9:443"] = errors.New("timeout")
	h.p.probeOnce(context.Background())
	h.p.probeOnce(context.Background())
	if m := h.expect(t, "ServiceUnreachable", RungGuided, 1)[0]; !strings.Contains(m, "default/api") || !strings.Contains(m, "kube-proxy") {
		t.Fatalf("service: %s", m)
	}
	h.expect(t, "EgressBlocked", RungAlert, 1)
	if len(h.p.serviceTargets(context.Background())) != 1 {
		t.Fatal("only Services with ready endpoints are dialled")
	}
	ok := newProbeHarness(t, config.Probes{Services: true, EgressTarget: "203.0.113.9:443"}, svc("api", "10.96.0.20"), slice)
	ok.p.probeOnce(context.Background())
	ok.p.probeOnce(context.Background())
	ok.quiet(t)
	StartNetworkProbes(context.Background(), h.deps, config.Probes{}) // everything off: returns at once
}

func event(name, reason, obj, host, msg string) *corev1.Event {
	return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Reason: reason, Message: msg,
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: obj}, Source: corev1.EventSource{Host: host}, LastTimestamp: metav1.NewTime(testNow.Add(-time.Minute))}
}

// ISS-036: sandbox and CNI failures, grouped per node.
func TestCheckSandboxFailures(t *testing.T) {
	h := newFindingHarness(t,
		event("e1", "FailedCreatePodSandBox", "api-1", "node-b", "failed to set up sandbox: plugin type=calico failed: no IPs available"),
		event("e2", "NetworkNotReady", "api-2", "node-b", "network is not ready: cni config uninitialized"),
		event("e3", "BackOff", "api-3", "node-b", "back-off"))
	CheckSandboxFailures(context.Background(), h.deps)
	if m := h.expect(t, "PodSandboxFailed", RungGuided, 1)[0]; !strings.Contains(m, "node/node-b") || !strings.Contains(m, "2 pod(s)") {
		t.Fatalf("sandbox: %s", m)
	}
}

// ISS-036: kube-proxy or a CNI pod not ready, only where kube-system is
// watched (constraint 4).
func TestCheckSystemNetworkPods(t *testing.T) {
	proxy := pod("kube-proxy-x", func(p *corev1.Pod) {
		p.Namespace = "kube-system"
		p.Labels = map[string]string{"k8s-app": "kube-proxy"}
		p.Spec.NodeName = "node-c"
		readyFor(p, false, 10*time.Minute)
	})
	quiet := newFindingHarness(t, proxy)
	CheckSystemNetworkPods(context.Background(), quiet.deps)
	quiet.quiet(t)
	h := newFindingHarness(t, proxy)
	p := *h.deps.Policy()
	p.WatchNamespaces = policy.NamespaceSet("default", "kube-system")
	h.deps.Policies = policy.Static(&p)
	CheckSystemNetworkPods(context.Background(), h.deps)
	if m := h.expect(t, "NetworkPodDown", RungGuided, 1)[0]; !strings.Contains(m, "node `node-c`") {
		t.Fatalf("network pod: %s", m)
	}
}

// ISS-036: a Service whose port no ingress policy allows.
func TestCheckNetworkPolicyBlocks(t *testing.T) {
	backend := pod("api-1", func(p *corev1.Pod) {
		p.Labels = map[string]string{"app": "api"}
		p.Spec.Containers = []corev1.Container{{Name: "app", Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}}}}
	})
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"},
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}}}}
	tcp := corev1.ProtocolTCP
	pol := func(name string, ports ...networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
		rules := []networkingv1.NetworkPolicyIngressRule{}
		if ports != nil {
			rules = append(rules, networkingv1.NetworkPolicyIngressRule{Ports: ports})
		}
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}, Ingress: rules}}
	}
	p9000 := intstr.FromInt(9000)
	p8080 := intstr.FromInt(8080)
	pHTTP := intstr.FromString("http")
	p8000, end := intstr.FromInt(8000), int32(8100)
	for _, tc := range []struct {
		name    string
		pol     *networkingv1.NetworkPolicy
		blocked bool
	}{
		{"deny all", pol("deny-all"), true},
		{"wrong port", pol("other", networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &p9000}), true},
		{"numeric port", pol("num", networkingv1.NetworkPolicyPort{Port: &p8080}), false},
		{"named port", pol("named", networkingv1.NetworkPolicyPort{Port: &pHTTP}), false},
		{"port range", pol("range", networkingv1.NetworkPolicyPort{Port: &p8000, EndPort: &end}), false},
		{"protocol only", pol("proto", networkingv1.NetworkPolicyPort{Protocol: &tcp}), false},
	} {
		h := newFindingHarness(t, backend, svc, tc.pol)
		CheckNetworkPolicyBlocks(context.Background(), h.deps)
		n := 0
		if tc.blocked {
			n = 1
		}
		msgs := h.expect(t, "NetworkPolicyBlocksService", RungGuided, n)
		if tc.blocked && !strings.Contains(msgs[0], tc.pol.Name) {
			t.Errorf("%s: names the policy: %s", tc.name, msgs[0])
		}
	}
	unselected := newFindingHarness(t, backend, svc, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}}})
	CheckNetworkPolicyBlocks(context.Background(), unselected.deps)
	unselected.quiet(t)
}

// ISS-036: conntrack and CoreDNS from Prometheus; silent without it.
func TestNetworkPromChecks(t *testing.T) {
	h := newFindingHarness(t)
	h.deps.Metrics = &mockMetrics{vector: func(q string) ([]metrics.Sample, error) {
		switch {
		case strings.Contains(q, "conntrack"):
			return []metrics.Sample{{Labels: map[string]string{"instance": "10.0.0.7:9100"}, Value: 0.95}, {Labels: map[string]string{"node": "ok"}, Value: 0.2}}, nil
		case strings.Contains(q, "SERVFAIL"):
			return []metrics.Sample{{Value: 0.12}}, nil
		default:
			return []metrics.Sample{{Value: 0.9}}, nil
		}
	}}
	CheckConntrack(context.Background(), h.deps)
	CheckCoreDNS(context.Background(), h.deps)
	if m := h.expect(t, "ConntrackNearFull", RungAlert, 1)[0]; !strings.Contains(m, "95 percent") {
		t.Fatalf("conntrack: %s", m)
	}
	h.expect(t, "CoreDNSErrors", RungGuided, 1)
	h.expect(t, "CoreDNSSlow", RungGuided, 1)
	none := newFindingHarness(t)
	none.deps.Metrics = &mockMetrics{}
	CheckConntrack(context.Background(), none.deps)
	CheckCoreDNS(context.Background(), none.deps)
	none.quiet(t)
}

// ISS-036: an Ingress TLS secret that does not exist, read only with the
// opt-in secret grant, inside the ceiling.
func TestCheckIngressTLS(t *testing.T) {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "default"}, Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{
		{Hosts: []string{"shop.example.test"}, SecretName: "shop-tls"}, {Hosts: []string{"ok.example.test"}, SecretName: "ok-tls"}}}}
	have := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "ok-tls", Namespace: "default"}, Type: corev1.SecretTypeTLS}
	off := newFindingHarness(t, ing, have)
	checkIngressTLS(context.Background(), off.deps)
	off.quiet(t)
	h := newFindingHarness(t, ing, have)
	h.deps.TLSCertCheck = true
	checkIngressTLS(context.Background(), h.deps)
	if m := h.expect(t, "IngressTLSSecretMissing", RungGuided, 1)[0]; !strings.Contains(m, "shop-tls") {
		t.Fatalf("tls: %s", m)
	}
}

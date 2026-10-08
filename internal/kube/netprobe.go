package kube

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	"github.com/supersaiyane/auto-agent-k8s/internal/config"
	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// Network probes (PLAN-002 phase 14). Every node agent checks, from its
// own pod network, what workloads on that node depend on: cluster DNS
// (ISS-033), Services through kube-proxy and the CNI, and egress (ISS-036,
// opt-in). Pod readiness cannot show a node that resolves nothing or
// cannot reach a ClusterIP; a probe from that node can.

const (
	probeFailuresBeforeReport = 2 // one blip never pages
	dnsSlowAfter              = time.Second
	dialTimeout               = 2 * time.Second
	serviceProbeSample        = 20 // Services dialled per pass
)

// netProber runs the probes; resolve and dial are injected for tests.
type netProber struct {
	deps    *Deps
	cfg     config.Probes
	resolve func(ctx context.Context, host string) ([]string, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	fails   map[string]int // consecutive failures per probe key
	offset  int            // rotation through the Services
}

func newNetProber(deps *Deps, cfg config.Probes) *netProber {
	d := &net.Dialer{Timeout: dialTimeout}
	return &netProber{deps: deps, cfg: cfg, resolve: net.DefaultResolver.LookupHost, dial: d.DialContext, fails: map[string]int{}}
}

// StartNetworkProbes runs the probes every interval until ctx ends. Node
// agents call it: each probes from its own node.
func StartNetworkProbes(ctx context.Context, deps *Deps, cfg config.Probes) {
	if !cfg.DNS && !cfg.Services && cfg.EgressTarget == "" {
		return
	}
	p := newNetProber(deps, cfg)
	klog.Infof("netprobe: dns=%v services=%v egress=%q every %s", cfg.DNS, cfg.Services, cfg.EgressTarget, cfg.Interval)
	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.probeOnce(ctx)
			}
		}
	}()
}

// probeOnce runs one pass of every enabled probe.
func (p *netProber) probeOnce(ctx context.Context) {
	if p.cfg.DNS {
		p.probeDNS(ctx, "kubernetes.default.svc."+p.cfg.ClusterDomain, true)
		if p.cfg.ExternalName != "" {
			p.probeDNS(ctx, p.cfg.ExternalName, false)
		}
	}
	if p.cfg.Services {
		p.probeServices(ctx)
	}
	if p.cfg.EgressTarget != "" {
		p.probeEgress(ctx)
	}
}

// failed counts a failure and reports whether it is time to tell someone.
func (p *netProber) failed(key string) bool {
	p.fails[key]++
	return p.fails[key] == probeFailuresBeforeReport
}

func (p *netProber) ok(key string) { delete(p.fails, key) }

func (p *netProber) nodeFinding(reason string, sev eventsvc.Severity, rung Rung, subject, summary, fix string) finding {
	return finding{Reason: reason, Workload: "node/" + p.deps.NodeName, Node: p.deps.NodeName, Severity: sev, Rung: rung,
		Subject: subject, Summary: summary, Fix: fix}
}

// closeProbe closes a probe connection; a failed close only matters at V(4).
func closeProbe(c net.Conn, addr string) {
	if err := c.Close(); err != nil {
		klog.V(4).Infof("netprobe: closing %s: %v", addr, err)
	}
}

// probeDNS resolves one name; internal names test cluster DNS, an external
// name tests the upstream resolvers.
func (p *netProber) probeDNS(ctx context.Context, host string, internal bool) {
	start := p.deps.clock()
	rctx, cancel := context.WithTimeout(ctx, dialTimeout)
	_, err := p.resolve(rctx, host)
	cancel()
	took := p.deps.clock().Sub(start)
	failKey, slowKey := "dns:"+host, "dnsslow:"+host
	switch {
	case err != nil:
		if p.failed(failKey) {
			fix := "check CoreDNS (and NodeLocal DNSCache) and this node's network: pods here cannot resolve Service names"
			rung := RungGuided
			if !internal {
				fix, rung = "the upstream DNS servers do not answer; check the cluster's forwarders and the node's resolvers", RungAlert
			}
			report(ctx, p.deps, p.nodeFinding("DNSResolutionFailed", eventsvc.SevCritical, rung, host,
				fmt.Sprintf("cannot resolve `%s` from this node, %d times in a row: %v", host, probeFailuresBeforeReport, err), fix))
		}
	case took > dnsSlowAfter:
		p.ok(failKey)
		if p.failed(slowKey) {
			report(ctx, p.deps, p.nodeFinding("DNSSlow", eventsvc.SevWarning, RungGuided, host,
				fmt.Sprintf("resolving `%s` took %s, %d times in a row", host, took.Round(time.Millisecond), probeFailuresBeforeReport),
				"look at CoreDNS latency and load, and at NodeLocal DNSCache on this node"))
		}
	default:
		p.ok(failKey)
		p.ok(slowKey)
	}
}

// probeTarget is one Service port to dial.
type probeTarget struct{ key, svc, addr string }

// serviceTargets lists one TCP port of every Service with ready endpoints
// in the watch scope, sorted.
func (p *netProber) serviceTargets(ctx context.Context) []probeTarget {
	var targets []probeTarget
	for _, ns := range watchedNamespaces(ctx, p.deps) {
		svcs, err := p.deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "services", ns)
			continue
		}
		ready, err := readyEndpointsByService(ctx, p.deps, ns)
		if err != nil {
			countAPIError(err, "endpointslices", ns)
			continue
		}
		for i := range svcs.Items {
			s := &svcs.Items[i]
			if ip := s.Spec.ClusterIP; ip == "" || ip == corev1.ClusterIPNone || ready[s.Name] == 0 {
				continue
			}
			for _, port := range s.Spec.Ports {
				if port.Protocol == "" || port.Protocol == corev1.ProtocolTCP {
					addr := net.JoinHostPort(s.Spec.ClusterIP, strconv.Itoa(int(port.Port)))
					targets = append(targets, probeTarget{ns + "/" + s.Name + ":" + strconv.Itoa(int(port.Port)), ns + "/" + s.Name, addr})
					break // one port per Service is enough to test the path
				}
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].key < targets[j].key })
	return targets
}

// probeServices dials a rotating sample of Service ClusterIPs. A failure
// from this node while the Service has ready endpoints points at kube-proxy
// or the CNI on this node.
func (p *netProber) probeServices(ctx context.Context) {
	targets := p.serviceTargets(ctx)
	for i := 0; i < len(targets) && i < serviceProbeSample; i++ {
		t := targets[(p.offset+i)%len(targets)]
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, err := p.dial(dctx, "tcp", t.addr)
		cancel()
		if err != nil {
			if p.failed("svc:" + t.key) {
				report(ctx, p.deps, p.nodeFinding("ServiceUnreachable", eventsvc.SevCritical, RungGuided, t.svc,
					fmt.Sprintf("cannot reach Service `%s` from this node although it has ready endpoints: %v", t.svc, err),
					"pods on this node cannot use the Service: check kube-proxy (or the CNI's Service handling) on this node"))
			}
			continue
		}
		closeProbe(conn, t.addr)
		p.ok("svc:" + t.key)
	}
	if len(targets) > 0 {
		p.offset = (p.offset + serviceProbeSample) % len(targets)
	}
}

// probeEgress dials the configured outside address.
func (p *netProber) probeEgress(ctx context.Context) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := p.dial(dctx, "tcp", p.cfg.EgressTarget)
	cancel()
	if err != nil {
		if p.failed("egress") {
			report(ctx, p.deps, p.nodeFinding("EgressBlocked", eventsvc.SevWarning, RungAlert, p.cfg.EgressTarget,
				fmt.Sprintf("cannot reach `%s` from this node: %v", p.cfg.EgressTarget, err),
				"check egress NetworkPolicies, the node's routes, NAT gateway and firewall"))
		}
		return
	}
	closeProbe(conn, p.cfg.EgressTarget)
	p.ok("egress")
}

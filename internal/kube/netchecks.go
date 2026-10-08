package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// Network checks on the leader (PLAN-002 phase 14, ISS-036).

// CheckSandboxFailures reports nodes where pods cannot get a sandbox or a
// network (events FailedCreatePodSandBox and NetworkNotReady), which means
// the CNI on that node is failing. One finding per node, naming the pods.
func CheckSandboxFailures(ctx context.Context, deps *Deps) {
	byNode := map[string][]string{}
	sample := map[string]string{}
	for _, ns := range watchedNamespaces(ctx, deps) {
		for _, ev := range recentEvents(ctx, deps, ns) {
			if ev.Reason != "FailedCreatePodSandBox" && ev.Reason != "NetworkNotReady" {
				continue
			}
			node := ev.Source.Host
			if node == "" {
				node = "unknown"
			}
			byNode[node] = append(byNode[node], ns+"/"+ev.InvolvedObject.Name)
			sample[node] = ev.Message
		}
	}
	for node, pods := range byNode {
		sort.Strings(pods)
		report(ctx, deps, finding{Reason: "PodSandboxFailed", Workload: "node/" + node, Node: node,
			Severity: eventsvc.SevCritical, Rung: RungGuided, Target: RungApprove,
			Summary: fmt.Sprintf("%d pod(s) cannot get a network on this node", len(pods)),
			Details: []string{"Pods: " + strings.Join(dedupe(pods), ", "), "Latest: " + trunc(sample[node], 200)},
			Fix:     "the CNI on this node is failing: check its pod there (Calico, Cilium, Flannel, aws-node), the node's IP pool and the CNI config"})
	}
}

// systemNetworkPods are the labels of kube-proxy and the common CNI agents.
func systemNetworkPods() []string {
	return []string{"k8s-app=kube-proxy", "k8s-app=calico-node", "k8s-app=cilium", "app=flannel", "k8s-app=aws-node",
		"name=weave-net", "app=antrea", "app=kindnet"}
}

// CheckSystemNetworkPods reports kube-proxy or CNI pods not ready for
// stuckFor, per node. They live in kube-system, which is outside the default
// watch scope, and reads follow the watch scope (constraint 4), so this runs
// only where kube-system is watched; the Service probe catches the same
// failure from the workloads' side without reading kube-system.
func CheckSystemNetworkPods(ctx context.Context, deps *Deps) {
	const ns = "kube-system"
	if !deps.Policy().Watched(ns) {
		return
	}
	now := deps.clock()
	for _, sel := range systemNetworkPods() {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			countAPIError(err, "pods", ns)
			return
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if since, ready := notReadyFor(p, now); ready || since < stuckFor {
				continue
			}
			report(ctx, deps, finding{Reason: "NetworkPodDown", Namespace: ns, Workload: ownerName(p), Pod: p.Name, Node: p.Spec.NodeName,
				Severity: eventsvc.SevCritical, Rung: RungGuided, Target: RungApprove, Subject: p.Spec.NodeName,
				Summary: fmt.Sprintf("`%s` (%s) on node `%s` is not ready: pod networking or Services fail there", p.Name, sel, p.Spec.NodeName),
				Details: []string{"State: " + waitingState(p)},
				Fix:     fmt.Sprintf("`kubectl -n kube-system describe pod %s`; deleting it recreates it on that node", p.Name)})
		}
	}
}

// CheckNetworkPolicyBlocks reports Services whose port no NetworkPolicy
// allows in: every pod behind the Service is selected by an ingress policy
// and no rule of those policies allows the target port. It is static: a
// rule that allows the port from some source counts as allowed.
func CheckNetworkPolicyBlocks(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		pols, err := deps.Client.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "networkpolicies", ns)
			continue
		}
		if len(pols.Items) == 0 {
			continue
		}
		svcs, err := deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "services", ns)
			continue
		}
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "pods", ns)
			continue
		}
		for i := range svcs.Items {
			for _, f := range policyBlocks(&svcs.Items[i], pods.Items, pols.Items) {
				report(ctx, deps, f)
			}
		}
	}
}

// policyBlocks lists the ports of one Service that its pods' ingress
// policies allow from nowhere.
func policyBlocks(svc *corev1.Service, pods []corev1.Pod, pols []networkingv1.NetworkPolicy) []finding {
	if len(svc.Spec.Selector) == 0 {
		return nil
	}
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	var backing []*corev1.Pod
	for i := range pods {
		if sel.Matches(labels.Set(pods[i].Labels)) {
			backing = append(backing, &pods[i])
		}
	}
	if len(backing) == 0 {
		return nil // CheckServiceEndpoints reports a selector that matches nothing
	}
	var out []finding
	for _, port := range svc.Spec.Ports {
		blocked, names := true, map[string]bool{}
		for _, p := range backing {
			applied := ingressPolicies(p, pols)
			if len(applied) == 0 || portAllowed(port.TargetPort, p, applied) {
				blocked = false
				break
			}
			for _, pol := range applied {
				names[pol.Name] = true
			}
		}
		if !blocked {
			continue
		}
		var polNames []string
		for n := range names {
			polNames = append(polNames, n)
		}
		sort.Strings(polNames)
		out = append(out, finding{Reason: "NetworkPolicyBlocksService", Namespace: svc.Namespace, Workload: "service/" + svc.Name,
			Subject: port.TargetPort.String(), Severity: eventsvc.SevCritical, Rung: RungGuided,
			Summary: fmt.Sprintf("no NetworkPolicy allows traffic in to port %s, so the Service drops every connection", port.TargetPort.String()),
			Details: []string{"Policies selecting its pods: " + strings.Join(polNames, ", ")},
			Fix:     fmt.Sprintf("add an ingress rule for port %s to one of these policies, from the clients that need it", port.TargetPort.String())})
	}
	return out
}

// ingressPolicies are the policies that select the pod and restrict ingress.
func ingressPolicies(p *corev1.Pod, pols []networkingv1.NetworkPolicy) []*networkingv1.NetworkPolicy {
	var out []*networkingv1.NetworkPolicy
	for i := range pols {
		pol := &pols[i]
		sel, err := metav1.LabelSelectorAsSelector(&pol.Spec.PodSelector)
		if err != nil || !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		ingress := len(pol.Spec.PolicyTypes) == 0 // no types: ingress is implied
		for _, t := range pol.Spec.PolicyTypes {
			if t == networkingv1.PolicyTypeIngress {
				ingress = true
			}
		}
		if ingress {
			out = append(out, pol)
		}
	}
	return out
}

// portAllowed reports whether any ingress rule of the policies allows the
// target port of this pod (by number or by container port name).
func portAllowed(target intstr.IntOrString, p *corev1.Pod, pols []*networkingv1.NetworkPolicy) bool {
	num, name := resolvePort(target, p)
	for _, pol := range pols {
		for _, rule := range pol.Spec.Ingress {
			if len(rule.Ports) == 0 {
				return true // every port
			}
			for _, rp := range rule.Ports {
				switch {
				case rp.Port == nil:
					return true
				case rp.Port.Type == intstr.String && rp.Port.StrVal == name && name != "":
					return true
				case rp.Port.Type == intstr.Int && num != 0 && int32(rp.Port.IntValue()) <= num &&
					(num == int32(rp.Port.IntValue()) || (rp.EndPort != nil && num <= *rp.EndPort)):
					return true
				}
			}
		}
	}
	return false
}

// resolvePort turns a Service target port into the pod's port number and
// name, through the containers' named ports.
func resolvePort(target intstr.IntOrString, p *corev1.Pod) (int32, string) {
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if (target.Type == intstr.String && cp.Name == target.StrVal) || (target.Type == intstr.Int && cp.ContainerPort == int32(target.IntValue())) {
				return cp.ContainerPort, cp.Name
			}
		}
	}
	if target.Type == intstr.Int {
		return int32(target.IntValue()), ""
	}
	return 0, target.StrVal
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// checkIngressTLS reports Ingress TLS entries whose secret does not exist.
// It reads TLS secret names, so it runs only with the opt-in secret read
// (TLS_CERT_CHECK) and only inside the fix ceiling, where that grant is.
func checkIngressTLS(ctx context.Context, deps *Deps) {
	if !deps.TLSCertCheck {
		return
	}
	for _, ns := range watchedNamespaces(ctx, deps) {
		if !deps.Policy().InCeiling(ns) {
			continue
		}
		ings, err := deps.Client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil || len(ings.Items) == 0 {
			if err != nil {
				countAPIError(err, "ingresses", ns)
			}
			continue
		}
		secrets, err := deps.Client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=kubernetes.io/tls"})
		if err != nil {
			countAPIError(err, "secrets", ns)
			continue
		}
		have := map[string]bool{}
		for i := range secrets.Items {
			have[secrets.Items[i].Name] = true
		}
		for i := range ings.Items {
			for _, t := range ings.Items[i].Spec.TLS {
				if t.SecretName == "" || have[t.SecretName] {
					continue
				}
				report(ctx, deps, finding{Reason: "IngressTLSSecretMissing", Namespace: ns, Workload: "ingress/" + ings.Items[i].Name,
					Subject: t.SecretName, Severity: eventsvc.SevCritical, Rung: RungGuided,
					Summary: fmt.Sprintf("TLS secret `%s` for %s does not exist, so HTTPS fails or falls back to a default certificate", t.SecretName, strings.Join(t.Hosts, ", ")),
					Fix:     "create the secret, or check the cert-manager Certificate that should issue it (`kubectl describe certificate -n " + ns + "`)"})
			}
		}
	}
}

// Prometheus network checks (PLAN-002 phase 14); silent without PromQL.
const (
	conntrackFullRatio = 0.9
	coreDNSErrorRatio  = 0.05
	coreDNSSlowSeconds = 0.5
)

// CheckConntrack reports nodes whose connection tracking table is nearly
// full (node-exporter); new connections are dropped once it fills.
func CheckConntrack(ctx context.Context, deps *Deps) {
	v, ok := queryVector(ctx, deps, "ConntrackNearFull", `node_nf_conntrack_entries / node_nf_conntrack_entries_limit`)
	if !ok {
		return
	}
	for _, s := range v {
		if s.Value < conntrackFullRatio {
			continue
		}
		node := firstLabel(s.Labels, "node", "instance")
		report(ctx, deps, finding{Reason: "ConntrackNearFull", Workload: "node/" + node, Node: node,
			Severity: eventsvc.SevWarning, Rung: RungAlert,
			Summary: fmt.Sprintf("the connection tracking table is %.0f percent full; new connections are dropped when it fills", s.Value*100),
			Fix:     "find the pods opening many short connections; raise net.netfilter.nf_conntrack_max on the node if the load is expected"})
	}
}

// CheckCoreDNS reports CoreDNS answering SERVFAIL too often, or slowly.
func CheckCoreDNS(ctx context.Context, deps *Deps) {
	if v, ok := queryVector(ctx, deps, "CoreDNSErrors",
		`sum(rate(coredns_dns_responses_total{rcode="SERVFAIL"}[5m])) / sum(rate(coredns_dns_responses_total[5m]))`); ok {
		for _, s := range v {
			if s.Value >= coreDNSErrorRatio {
				report(ctx, deps, finding{Reason: "CoreDNSErrors", Workload: "coredns", Severity: eventsvc.SevCritical, Rung: RungGuided,
					Summary: fmt.Sprintf("%.1f percent of DNS answers are SERVFAIL", s.Value*100),
					Fix:     "read the CoreDNS logs; SERVFAIL usually means the upstream resolvers fail or a forward zone is wrong"})
			}
		}
	}
	if v, ok := queryVector(ctx, deps, "CoreDNSSlow",
		`histogram_quantile(0.99, sum(rate(coredns_dns_request_duration_seconds_bucket[5m])) by (le))`); ok {
		for _, s := range v {
			if s.Value >= coreDNSSlowSeconds {
				report(ctx, deps, finding{Reason: "CoreDNSSlow", Workload: "coredns", Severity: eventsvc.SevWarning, Rung: RungGuided,
					Summary: fmt.Sprintf("the 99th percentile DNS answer takes %.2fs", s.Value),
					Fix:     "scale CoreDNS, add NodeLocal DNSCache, or check the upstream resolvers' latency"})
			}
		}
	}
}

func firstLabel(l map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := l[k]; v != "" {
			return v
		}
	}
	return "unknown"
}

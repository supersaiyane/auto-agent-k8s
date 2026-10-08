package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/supersaiyane/auto-agent-k8s/internal/events"
)

// defaultClassAnnotation marks the cluster's default StorageClass.
const defaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"

// CheckStorageIssues reports claims whose volume is gone (Lost), pending
// claims that name a StorageClass that does not exist, and pending claims
// with no class when the cluster has no default class (phase 12 audit).
func CheckStorageIssues(ctx context.Context, deps *Deps) {
	classes, ok := storageClasses(ctx, deps)
	for _, ns := range watchedNamespaces(ctx, deps) {
		pvcs, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "persistentvolumeclaims", ns)
			continue
		}
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			switch {
			case pvc.Status.Phase == corev1.ClaimLost:
				report(ctx, deps, finding{Reason: "PVCLost", Namespace: ns, Workload: "pvc/" + pvc.Name,
					Severity: eventsvc.SevCritical, Rung: RungGuided,
					Summary: "the PersistentVolume behind this claim is gone; its data may be lost",
					Fix:     "restore the data from a backup into a new volume, then recreate the claim"})
			case pvc.Status.Phase == corev1.ClaimPending && ok:
				if f, bad := classProblem(pvc, classes); bad {
					report(ctx, deps, f)
				}
			}
		}
	}
}

// storageClasses lists the cluster's classes by name; a failed read is
// counted and the class checks are skipped rather than guessed.
func storageClasses(ctx context.Context, deps *Deps) (map[string]*storagev1.StorageClass, bool) {
	list, err := deps.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "storageclasses", "")
		return nil, false
	}
	out := map[string]*storagev1.StorageClass{}
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, true
}

// classProblem explains a pending claim's StorageClass trouble, if any. An
// empty class name asks for static binding and is not a class problem.
func classProblem(pvc *corev1.PersistentVolumeClaim, classes map[string]*storagev1.StorageClass) (finding, bool) {
	f := finding{Namespace: pvc.Namespace, Workload: "pvc/" + pvc.Name, Severity: eventsvc.SevCritical, Rung: RungGuided}
	if name := pvc.Spec.StorageClassName; name != nil {
		if *name == "" || classes[*name] != nil {
			return f, false
		}
		f.Reason = "StorageClassNotFound"
		f.Summary = fmt.Sprintf("names StorageClass `%s`, which does not exist", *name)
		f.Fix = "create that StorageClass, or recreate the claim with one from `kubectl get storageclass`"
		return f, true
	}
	for _, c := range classes {
		if c.Annotations[defaultClassAnnotation] == "true" {
			return f, false
		}
	}
	f.Reason = "NoDefaultStorageClass"
	f.Summary = "names no StorageClass and the cluster has no default one, so nothing will provision it"
	f.Fix = fmt.Sprintf("mark a class as default (`kubectl annotate storageclass <name> %s=true`) or set storageClassName on the claim", defaultClassAnnotation)
	return f, true
}

// CheckNetworkIssues reports CoreDNS down, LoadBalancers without an address
// and Ingress backends that cannot serve.
func CheckNetworkIssues(ctx context.Context, deps *Deps) {
	checkDNSHealth(ctx, deps)
	checkLoadBalancerPending(ctx, deps)
	checkIngressBackends(ctx, deps)
}

// checkDNSHealth reads the CoreDNS pods in kube-system. kube-system is
// outside the default watch scope, and reads follow the watch scope
// (constraint 4), so it runs only where kube-system is watched; the phase 12
// audit found it read there regardless. Resolution checks that need no pod
// reads are PLAN-002 phase 14 (ISS-033).
func checkDNSHealth(ctx context.Context, deps *Deps) {
	const ns = "kube-system"
	if !deps.Policy().Watched(ns) {
		return
	}
	var pods []corev1.Pod
	for _, sel := range []string{"k8s-app=kube-dns", "app.kubernetes.io/name=coredns"} {
		list, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			countAPIError(err, "pods", ns)
			return
		}
		if pods = list.Items; len(pods) > 0 {
			break
		}
	}
	ready := 0
	for i := range pods {
		if _, ok := notReadyFor(&pods[i], deps.clock()); ok {
			ready++
		}
	}
	f := finding{Namespace: ns, Workload: "coredns", Rung: RungGuided,
		Fix: "describe the CoreDNS pods (`kubectl -n kube-system describe pods -l k8s-app=kube-dns`) and read their logs"}
	switch {
	case len(pods) == 0 || ready == len(pods):
		return
	case ready == 0:
		f.Reason, f.Severity = "DNSDown", eventsvc.SevCritical
		f.Summary = fmt.Sprintf("no CoreDNS pod is ready (0/%d): name resolution fails for every pod", len(pods))
	default:
		f.Reason, f.Severity = "DNSDegraded", eventsvc.SevWarning
		f.Summary = fmt.Sprintf("%d/%d CoreDNS pods ready", ready, len(pods))
	}
	report(ctx, deps, f)
}

// lbPendingFor is how long a LoadBalancer Service may wait for an address.
const lbPendingFor = 5 * time.Minute

func checkLoadBalancerPending(ctx context.Context, deps *Deps) {
	now := deps.clock()
	for _, ns := range watchedNamespaces(ctx, deps) {
		svcs, err := deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "services", ns)
			continue
		}
		for i := range svcs.Items {
			svc := &svcs.Items[i]
			if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || len(svc.Status.LoadBalancer.Ingress) > 0 ||
				now.Sub(svc.CreationTimestamp.Time) < lbPendingFor {
				continue
			}
			report(ctx, deps, finding{Reason: "LoadBalancerPending", Namespace: ns, Workload: "service/" + svc.Name,
				Severity: eventsvc.SevWarning, Rung: RungAlert,
				Summary: fmt.Sprintf("no external address after %s", now.Sub(svc.CreationTimestamp.Time).Round(time.Minute)),
				Fix:     fmt.Sprintf("the cause is usually outside the cluster: cloud load balancer quota, the IP pool, or a missing controller; `kubectl describe service %s -n %s` shows its events", svc.Name, ns)})
		}
	}
}

// checkIngressBackends reports Ingress paths, and default backends, whose
// Service is missing or has no ready endpoint. Endpoints come from
// EndpointSlices (ISS-035). Resource backends are not Services and are
// skipped; the phase 12 audit found they crashed the check.
func checkIngressBackends(ctx context.Context, deps *Deps) {
	for _, ns := range watchedNamespaces(ctx, deps) {
		ingresses, err := deps.Client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "ingresses", ns)
			continue
		}
		if len(ingresses.Items) == 0 {
			continue
		}
		svcs, err := deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			countAPIError(err, "services", ns)
			continue
		}
		exists := map[string]bool{}
		for i := range svcs.Items {
			exists[svcs.Items[i].Name] = true
		}
		ready, err := readyEndpointsByService(ctx, deps, ns)
		if err != nil {
			countAPIError(err, "endpointslices", ns)
			continue
		}
		for i := range ingresses.Items {
			for _, b := range ingressBackends(&ingresses.Items[i]) {
				if f, bad := backendProblem(&ingresses.Items[i], b, exists, ready); bad {
					report(ctx, deps, f)
				}
			}
		}
	}
}

// ingressBackend is one Service an Ingress routes to, and where.
type ingressBackend struct{ service, where string }

func ingressBackends(ing *networkingv1.Ingress) []ingressBackend {
	var out []ingressBackend
	if d := ing.Spec.DefaultBackend; d != nil && d.Service != nil {
		out = append(out, ingressBackend{d.Service.Name, "default backend"})
	}
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, p := range rule.HTTP.Paths {
			if p.Backend.Service != nil && p.Backend.Service.Name != "" {
				out = append(out, ingressBackend{p.Backend.Service.Name, fmt.Sprintf("host %q path %q", rule.Host, p.Path)})
			}
		}
	}
	return out
}

func backendProblem(ing *networkingv1.Ingress, b ingressBackend, exists map[string]bool, ready map[string]int) (finding, bool) {
	f := finding{Namespace: ing.Namespace, Workload: "ingress/" + ing.Name, Subject: b.service,
		Severity: eventsvc.SevCritical, Rung: RungGuided, Details: []string{"Route: " + b.where}}
	switch {
	case !exists[b.service]:
		f.Reason = "IngressBackendMissing"
		f.Summary = fmt.Sprintf("routes to Service `%s`, which does not exist", b.service)
		f.Fix = "create the Service or correct the backend name in the Ingress"
	case ready[b.service] == 0:
		f.Reason = "IngressNoBackends"
		f.Summary = fmt.Sprintf("routes to Service `%s`, which has no ready endpoint: requests get 502 or 503", b.service)
		f.Fix = "see the NoEndpoints finding for that Service: its selector or its pods' readiness"
	default:
		return f, false
	}
	return f, true
}

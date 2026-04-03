package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsvc "github.com/yourorg/auto-agent/internal/events"
	"github.com/yourorg/auto-agent/internal/obs"
)

// CheckStorageIssues detects PVC Lost, VolumeAttachment stuck, and StorageClass problems.
func CheckStorageIssues(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		pvcs, err := deps.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, pvc := range pvcs.Items {
			switch pvc.Status.Phase {
			case corev1.ClaimLost:
				key := dedupKey(ns, pvc.Name, "PVCLost")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*PVCLost* `%s/%s` — underlying PersistentVolume was deleted\n", ns, pvc.Name)
				msg += "_Action required_: data may be lost. Restore from backup or create new PV.\n"
				deps.Slack.Post(msg)
				fireAlert(ctx, deps, "PVCLost", ns, pvc.Name, "", msg, "critical")
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
					Namespace: ns, Workload: pvc.Name, Reason: "PVCLost", Message: "PV deleted, data at risk"})
				obs.IncidentsTotal.WithLabelValues("PVCLost", ns, pvc.Name).Inc()

			case corev1.ClaimPending:
				// Already handled by CheckPendingPVCs, but check for StorageClass issues
				if pvc.Spec.StorageClassName != nil {
					scName := *pvc.Spec.StorageClassName
					_, err := deps.Client.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{})
					if err != nil {
						key := dedupKey(ns, pvc.Name, "StorageClassNotFound")
						if !deps.Dedup.Check(key) {
							continue
						}
						msg := fmt.Sprintf("*StorageClassNotFound* PVC `%s/%s` references StorageClass `%s` which doesn't exist\n",
							ns, pvc.Name, scName)
						msg += "_Fix_: create the StorageClass or update the PVC.\n"
						deps.Slack.Post(msg)
						recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
							Namespace: ns, Workload: pvc.Name, Reason: "StorageClassNotFound",
							Message: fmt.Sprintf("StorageClass %s not found", scName)})
						obs.IncidentsTotal.WithLabelValues("StorageClassNotFound", ns, pvc.Name).Inc()
					}
				}
			}
		}
	}
}

// CheckVolumeAttachments detects volumes stuck in attaching state.
func CheckVolumeAttachments(ctx context.Context, deps *Deps) {
	// Check pod events for volume-related failures
	for ns := range deps.Policy.NamespaceAllow {
		pods, err := deps.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodPending {
				continue
			}
			events := collectEvents(ctx, deps.Client, ns, pod.Name)
			for _, ev := range events {
				if strings.Contains(ev, "AttachVolume") && strings.Contains(ev, "failed") ||
					strings.Contains(ev, "Multi-Attach error") {
					key := dedupKey(ns, pod.Name, "VolumeAttachStuck")
					if !deps.Dedup.Check(key) {
						break
					}
					wl := ownerName(&pod)
					msg := fmt.Sprintf("*VolumeAttachStuck* pod `%s/%s` cannot attach volume\n", ns, pod.Name)
					msg += fmt.Sprintf("Event: %s\n", ev)
					msg += "_Check_: volume may be attached to another node (multi-attach), or wrong AZ.\n"
					deps.Slack.Post(msg)
					recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
						Namespace: ns, Workload: wl, Pod: pod.Name, Reason: "VolumeAttachStuck", Message: ev})
					obs.IncidentsTotal.WithLabelValues("VolumeAttachStuck", ns, wl).Inc()
					break
				}
			}
		}
	}
}

// CheckNetworkIssues detects DNS failures, LoadBalancer pending, and Ingress backend errors.
func CheckNetworkIssues(ctx context.Context, deps *Deps) {
	checkDNSHealth(ctx, deps)
	checkLoadBalancerPending(ctx, deps)
	checkIngressBackends(ctx, deps)
}

func checkDNSHealth(ctx context.Context, deps *Deps) {
	// Check CoreDNS pods in kube-system
	pods, err := deps.Client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "k8s-app=kube-dns",
	})
	if err != nil {
		return
	}
	if pods == nil || len(pods.Items) == 0 {
		// Try coredns label
		pods, err = deps.Client.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=coredns",
		})
		if err != nil || pods == nil {
			return
		}
	}

	totalDNS := len(pods.Items)
	readyDNS := 0
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				readyDNS++
				break
			}
		}
	}
	if readyDNS == 0 && totalDNS > 0 {
		key := dedupKey("kube-system", "coredns", "DNSDown")
		if !deps.Dedup.Check(key) {
			return
		}
		msg := fmt.Sprintf("*DNSDown* — ALL CoreDNS pods are down (%d/%d ready)\n", readyDNS, totalDNS)
		msg += "_CRITICAL_: cluster DNS resolution will fail for all pods.\n"
		deps.Slack.Post(msg)
		fireAlert(ctx, deps, "DNSDown", "kube-system", "coredns", "", msg, "critical")
		recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
			Namespace: "kube-system", Workload: "coredns", Reason: "DNSDown",
			Message: fmt.Sprintf("0/%d DNS pods ready", totalDNS)})
		obs.IncidentsTotal.WithLabelValues("DNSDown", "kube-system", "coredns").Inc()
	} else if readyDNS < totalDNS {
		key := dedupKey("kube-system", "coredns", "DNSDegraded")
		if !deps.Dedup.Check(key) {
			return
		}
		msg := fmt.Sprintf("*DNSDegraded* — CoreDNS partially down (%d/%d ready)\n", readyDNS, totalDNS)
		deps.Slack.Post(msg)
		recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
			Namespace: "kube-system", Workload: "coredns", Reason: "DNSDegraded",
			Message: fmt.Sprintf("%d/%d DNS pods ready", readyDNS, totalDNS)})
		obs.IncidentsTotal.WithLabelValues("DNSDegraded", "kube-system", "coredns").Inc()
	}
}

func checkLoadBalancerPending(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		svcs, err := deps.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, svc := range svcs.Items {
			if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
				continue
			}
			if len(svc.Status.LoadBalancer.Ingress) == 0 {
				// Check if pending for >5 min
				if time.Since(svc.CreationTimestamp.Time) < 5*time.Minute {
					continue
				}
				key := dedupKey(ns, svc.Name, "LoadBalancerPending")
				if !deps.Dedup.Check(key) {
					continue
				}
				msg := fmt.Sprintf("*LoadBalancerPending* `%s/%s` has no external IP (>5 min)\n", ns, svc.Name)
				msg += "_Check_: cloud provider LB quota, IP pool exhaustion, or service annotations.\n"
				deps.Slack.Post(msg)
				recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevWarning,
					Namespace: ns, Workload: svc.Name, Reason: "LoadBalancerPending",
					Message: "No external IP assigned"})
				obs.IncidentsTotal.WithLabelValues("LoadBalancerPending", ns, svc.Name).Inc()
			}
		}
	}
}

func checkIngressBackends(ctx context.Context, deps *Deps) {
	for ns := range deps.Policy.NamespaceAllow {
		ingresses, err := deps.Client.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, ing := range ingresses.Items {
			for _, rule := range ing.Spec.Rules {
				if rule.HTTP == nil {
					continue
				}
				for _, path := range rule.HTTP.Paths {
					svcName := path.Backend.Service.Name
					if svcName == "" {
						continue
					}
					// Check if the backend service has endpoints
					ep, err := deps.Client.CoreV1().Endpoints(ns).Get(ctx, svcName, metav1.GetOptions{})
					if err != nil {
						key := dedupKey(ns, ing.Name, "IngressBackendMissing-"+svcName)
						if deps.Dedup.Check(key) {
							msg := fmt.Sprintf("*IngressBackendMissing* ingress `%s/%s` backend `%s` not found\n",
								ns, ing.Name, svcName)
							msg += fmt.Sprintf("Host: %s, Path: %s\n", rule.Host, path.Path)
							msg += "_Fix_: create the backend service or update ingress rules.\n"
							deps.Slack.Post(msg)
							recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
								Namespace: ns, Workload: ing.Name, Reason: "IngressBackendMissing",
								Message: fmt.Sprintf("Backend %s not found", svcName)})
							obs.IncidentsTotal.WithLabelValues("IngressBackendMissing", ns, ing.Name).Inc()
						}
						continue
					}
					readyCount := 0
					for _, subset := range ep.Subsets {
						readyCount += len(subset.Addresses)
					}
					if readyCount == 0 {
						key := dedupKey(ns, ing.Name, "IngressNoBackends-"+svcName)
						if deps.Dedup.Check(key) {
							msg := fmt.Sprintf("*IngressNoBackends* ingress `%s/%s` backend `%s` has 0 ready endpoints\n",
								ns, ing.Name, svcName)
							msg += fmt.Sprintf("Host: %s — requests to this path will get 502/503.\n", rule.Host)
							deps.Slack.Post(msg)
							recordEvent(deps, eventsvc.Event{Type: eventsvc.Incident, Severity: eventsvc.SevCritical,
								Namespace: ns, Workload: ing.Name, Reason: "IngressNoBackends",
								Message: fmt.Sprintf("Backend %s: 0 endpoints", svcName)})
							obs.IncidentsTotal.WithLabelValues("IngressNoBackends", ns, ing.Name).Inc()
						}
					}
				}
			}
		}
	}
}

// checkCertExpiry is in security.go

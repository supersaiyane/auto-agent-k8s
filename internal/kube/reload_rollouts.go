package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/klog/v2"
)

// Argo Rollouts (PLAN-002 A4). A Rollout carries a Deployment-shaped pod
// template, so config reload treats it like a Deployment, through the
// dynamic client, and only when the Rollout CRD is installed.

var rolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

// DetectRollouts reports whether the cluster serves Argo Rollouts. A
// missing group is the normal answer on most clusters, not an error.
func DetectRollouts(d discovery.DiscoveryInterface) bool {
	list, err := d.ServerResourcesForGroupVersion(rolloutGVR.GroupVersion().String())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			klog.Warningf("rollouts: discovery failed, Argo Rollouts support off: %v", err)
		}
		return false
	}
	for _, r := range list.APIResources {
		if r.Name == rolloutGVR.Resource {
			klog.Infof("rollouts: Argo Rollouts found; config reload covers Rollouts")
			return true
		}
	}
	return false
}

// rolloutTemplate is one Rollout's metadata and pod template.
type rolloutTemplate struct {
	meta     metav1.ObjectMeta
	template *corev1.PodTemplateSpec
}

// rollouts lists the Rollouts in ns that hold their own pod template. One
// that points at a Deployment (spec.workloadRef) is skipped: its template
// lives in that Deployment, which the reloader already restarts.
func (r *Reloader) rollouts(ctx context.Context, ns string) []rolloutTemplate {
	if !r.deps.Rollouts || r.deps.Dynamic == nil {
		return nil
	}
	l, err := r.deps.Dynamic.Resource(rolloutGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		countAPIError(err, "rollouts", ns)
		return nil
	}
	var out []rolloutTemplate
	for i := range l.Items {
		u := &l.Items[i]
		if _, found, _ := unstructured.NestedMap(u.Object, "spec", "workloadRef"); found {
			continue
		}
		raw, found, err := unstructured.NestedMap(u.Object, "spec", "template")
		if err != nil || !found {
			klog.V(3).Infof("rollouts: %s/%s has no pod template, skipped", ns, u.GetName())
			continue
		}
		var t corev1.PodTemplateSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &t); err != nil {
			klog.Warningf("rollouts: %s/%s: reading the pod template: %v", ns, u.GetName(), err)
			continue
		}
		out = append(out, rolloutTemplate{meta: metav1.ObjectMeta{Name: u.GetName(), Namespace: ns, Annotations: u.GetAnnotations()}, template: &t})
	}
	return out
}

// verifyRollout reads Argo's own verdict. Healthy is done; Degraded failed;
// Paused is a canary waiting for promotion, which Argo now owns, so the
// wave moves on; anything else is still rolling out.
func (r *Reloader) verifyRollout(ctx context.Context, ns, name string, timedOut bool) (done, ok bool, detail string) {
	u, err := r.deps.Dynamic.Resource(rolloutGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		countAPIError(err, "rollouts", ns)
		return rolloutDone(false, timedOut, "could not read the Rollout")
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	switch phase {
	case "Healthy":
		return true, true, "Rollout healthy"
	case "Degraded":
		msg, _, _ := unstructured.NestedString(u.Object, "status", "message")
		return true, false, fmt.Sprintf("Rollout degraded: %s", msg)
	case "Paused":
		return true, true, "Rollout paused for promotion; Argo Rollouts carries it from here"
	}
	return rolloutDone(false, timedOut, fmt.Sprintf("Rollout %s", phaseOr(phase)))
}

func phaseOr(p string) string {
	if p == "" {
		return "has no status yet"
	}
	return "is " + p
}

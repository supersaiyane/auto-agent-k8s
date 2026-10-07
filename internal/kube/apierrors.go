package kube

import (
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

	"github.com/yourorg/auto-agent/internal/obs"
)

var warnedForbidden sync.Map // resource -> struct{}

// countAPIError records a failed API read instead of dropping it. Forbidden
// means the chart does not grant what a detector needs (ISS-009): it is
// counted and logged once per resource at warning level, so a missing grant
// shows up instead of a detector silently never running.
func countAPIError(err error, resource, ns string) {
	reason := "other"
	switch {
	case apierrors.IsForbidden(err):
		reason = "forbidden"
	case apierrors.IsNotFound(err):
		reason = "not_found"
	}
	obs.APIErrorsTotal.WithLabelValues(resource, reason).Inc()
	if reason == "forbidden" {
		if _, seen := warnedForbidden.LoadOrStore(resource, struct{}{}); !seen {
			klog.Warningf("api: forbidden to read %s (namespace %q); this detector is disabled until RBAC grants it: %v", resource, ns, err)
		}
		return
	}
	klog.V(3).Infof("api: read %s in %q failed: %v", resource, ns, err)
}

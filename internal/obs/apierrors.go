package obs

import (
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
)

var warnedForbidden sync.Map // resource -> struct{}

// CountAPIError records a failed Kubernetes API read instead of dropping it.
// Forbidden means RBAC does not grant what the caller needs (ISS-009): it is
// counted and logged once per resource at warning level, so a missing grant
// shows up instead of a feature silently returning nothing.
func CountAPIError(err error, resource, ns string) {
	reason := "other"
	switch {
	case apierrors.IsForbidden(err):
		reason = "forbidden"
	case apierrors.IsNotFound(err):
		reason = "not_found"
	}
	APIErrorsTotal.WithLabelValues(resource, reason).Inc()
	if reason == "forbidden" {
		if _, seen := warnedForbidden.LoadOrStore(resource, struct{}{}); !seen {
			klog.Warningf("api: forbidden to read %s (namespace %q); this detector is disabled until RBAC grants it: %v", resource, ns, err)
		}
		return
	}
	klog.V(3).Infof("api: read %s in %q failed: %v", resource, ns, err)
}

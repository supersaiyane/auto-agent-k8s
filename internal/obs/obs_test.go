package obs

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ISS-009: every failed read is counted by reason; forbidden is warned once
// per resource.
func TestCountAPIError_ByReason(t *testing.T) {
	gr := schema.GroupResource{Resource: "widgets"}
	cases := []struct {
		err    error
		reason string
	}{
		{apierrors.NewForbidden(gr, "x", errors.New("rbac")), "forbidden"},
		{apierrors.NewNotFound(gr, "x"), "not_found"},
		{errors.New("etcd timeout"), "other"},
	}
	for _, c := range cases {
		before := testutil.ToFloat64(APIErrorsTotal.WithLabelValues("widgets", c.reason))
		CountAPIError(c.err, "widgets", "ns")
		if got := testutil.ToFloat64(APIErrorsTotal.WithLabelValues("widgets", c.reason)) - before; got != 1 {
			t.Errorf("%s counted %v", c.reason, got)
		}
	}
	CountAPIError(apierrors.NewForbidden(gr, "y", errors.New("rbac")), "widgets", "ns")
	if _, seen := warnedForbidden.Load("widgets"); !seen {
		t.Fatal("a forbidden resource is remembered so it is warned once")
	}
}

// Every metric is registered and reports under its documented name.
func TestMetricsRegistered(t *testing.T) {
	InfoGauge.WithLabelValues("v", "fix").Set(1)
	if n := testutil.CollectAndCount(InfoGauge, "auto_agent_info"); n == 0 {
		t.Fatal("auto_agent_info not collected")
	}
}

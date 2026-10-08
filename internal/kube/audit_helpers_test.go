package kube

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/obs"
)

// Shared helpers for the phase 12 detector audit (ISS-039).

// forbid makes every list of resource fail with Forbidden.
func forbid(t *testing.T, deps *Deps, resource string) {
	t.Helper()
	kc, ok := deps.Client.(*fake.Clientset)
	if !ok {
		t.Fatal("forbid needs a fake clientset")
	}
	kc.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("rbac"))
	})
}

// forbidden reads the forbidden counter for resource.
func forbidden(resource string) float64 {
	return testutil.ToFloat64(obs.APIErrorsTotal.WithLabelValues(resource, "forbidden"))
}

// expectCounted runs check with resource forbidden and asserts the error is
// counted, never dropped (CLAUDE.md definition of done).
func expectCounted(t *testing.T, resource string, check func(*Deps)) {
	t.Helper()
	h := newFindingHarness(t)
	forbid(t, h.deps, resource)
	before := forbidden(resource)
	check(h.deps)
	if forbidden(resource) <= before {
		t.Fatalf("a forbidden %s list was not counted", resource)
	}
}

// quiet asserts the harness saw no finding at all.
func (h *findingHarness) quiet(t *testing.T) {
	t.Helper()
	if n := len(h.rec.Recent(100)); n != 0 || len(h.slack.Messages()) != 0 {
		t.Fatalf("expected no finding, got %d events: %v", n, h.slack.Messages())
	}
}

// recordingDuring wraps a client and runs during on each apps read, to put
// a concurrent change in the middle of a pass.
type recordingDuring struct {
	kubernetes.Interface
	during func()
}

func (r *recordingDuring) AppsV1() appsv1client.AppsV1Interface {
	r.during()
	return r.Interface.AppsV1()
}

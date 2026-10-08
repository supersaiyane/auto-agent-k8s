package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/supersaiyane/auto-agent-k8s/internal/policy"
)

// ADR-002: with every namespace watched, the detectors read the namespaces
// the API server lists, minus the system ones; a failed list reads nothing.
func TestWatchedNamespaces(t *testing.T) {
	ctx := context.Background()
	deps, _ := newHandlerTestDeps(t)
	if got := strings.Join(watchedNamespaces(ctx, deps), ","); got != "default" {
		t.Fatalf("explicit list: %s", got)
	}

	var objs []runtime.Object
	for _, n := range []string{"payments", "kube-system", "auto-agent", "billing"} {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}
	kc := fake.NewSimpleClientset(objs...)
	deps.Client = kc
	deps.Policies = policy.Static(policy.Load(func(k string) string {
		return map[string]string{"POD_NAMESPACE": "auto-agent"}[k]
	}))
	if got := strings.Join(watchedNamespaces(ctx, deps), ","); got != "billing,payments" {
		t.Fatalf("watch all: %s", got)
	}

	kc.PrependReactor("list", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	if got := watchedNamespaces(ctx, deps); len(got) != 0 {
		t.Fatalf("a failed list reads nothing: %v", got)
	}
}

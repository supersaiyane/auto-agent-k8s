package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

var testMirror = Mirror{Prefix: "registry.internal/", AllowList: []string{"docker.io/library/", "quay.io/prometheus/"}}

func TestNormalizeImage(t *testing.T) {
	cases := map[string]string{
		"nginx:1.25":                    "docker.io/library/nginx:1.25",
		"bitnami/redis:7":               "docker.io/bitnami/redis:7",
		"docker.io/library/nginx":       "docker.io/library/nginx",
		"quay.io/prometheus/prometheus": "quay.io/prometheus/prometheus",
		"registry:5000/app":             "registry:5000/app",
		"localhost/app:1":               "localhost/app:1",
	}
	for in, want := range cases {
		if got := normalizeImage(in); got != want {
			t.Errorf("normalizeImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// ISS-012: only allowlisted images are rewritten, and only once.
func TestMirror_Rewrite(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"nginx:1.25", "registry.internal/docker.io/library/nginx:1.25", true},
		{"quay.io/prometheus/node-exporter:v1", "registry.internal/quay.io/prometheus/node-exporter:v1", true},
		{"bitnami/redis:7", "bitnami/redis:7", false},                                                         // docker.io/bitnami is not allowlisted
		{"ghcr.io/corp/app:1", "ghcr.io/corp/app:1", false},                                                   // another registry
		{"registry.internal/docker.io/library/nginx:1", "registry.internal/docker.io/library/nginx:1", false}, // already mirrored
	}
	for _, c := range cases {
		got, ok := testMirror.rewrite(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("rewrite(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if (Mirror{Prefix: "p/"}).Enabled() || (Mirror{AllowList: []string{"a"}}).Enabled() || !testMirror.Enabled() {
		t.Fatal("enabled needs a prefix and an allowlist")
	}
	if _, ok := (Mirror{Prefix: "p/", AllowList: []string{""}}).rewrite("nginx"); ok {
		t.Fatal("an empty allowlist entry matches nothing")
	}
}

func podReview(t *testing.T, pod corev1.Pod) []byte {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	review := admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1.AdmissionRequest{UID: types.UID("u1"), Namespace: "default", Kind: metav1.GroupVersionKind{Kind: "Pod"},
			Object: runtime.RawExtension{Raw: raw}}}
	b, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ISS-012: the webhook answers with a JSON patch for every allowlisted image
// in containers, init containers and ephemeral containers, and never denies.
func TestServeMutate(t *testing.T) {
	v := NewValidator(Config{Mirror: testMirror})
	pod := corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "init", Image: "busybox:1.36"}},
		Containers:     []corev1.Container{{Name: "app", Image: "ghcr.io/corp/app:1"}, {Name: "proxy", Image: "nginx:1.25"}},
		EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debug", Image: "quay.io/prometheus/busybox:latest"}}},
	}}
	rec := httptest.NewRecorder()
	v.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(podReview(t, pod))))
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Response == nil || !out.Response.Allowed || out.Response.UID != "u1" {
		t.Fatalf("response %s (%v)", rec.Body.String(), err)
	}
	var ops []patchOp
	if err := json.Unmarshal(out.Response.Patch, &ops); err != nil {
		t.Fatal(err)
	}
	want := []patchOp{
		{"replace", "/spec/initContainers/0/image", "registry.internal/docker.io/library/busybox:1.36"},
		{"replace", "/spec/containers/1/image", "registry.internal/docker.io/library/nginx:1.25"},
		{"replace", "/spec/ephemeralContainers/0/image", "registry.internal/quay.io/prometheus/busybox:latest"},
	}
	if len(ops) != len(want) {
		t.Fatalf("ops %+v", ops)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Errorf("op %d = %+v, want %+v", i, ops[i], want[i])
		}
	}
	if out.Response.PatchType == nil || *out.Response.PatchType != admissionv1.PatchTypeJSONPatch {
		t.Fatal("patch type JSONPatch")
	}

	untouched := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "ghcr.io/corp/app:1"}}}}
	rec = httptest.NewRecorder()
	v.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(podReview(t, untouched))))
	var plain admissionv1.AdmissionReview // fresh: decoding into out would keep its old patch
	if err := json.Unmarshal(rec.Body.Bytes(), &plain); err != nil || plain.Response.Patch != nil || !plain.Response.Allowed {
		t.Fatalf("nothing allowlisted: no patch, allowed: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	v.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader([]byte("junk"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatal("an undecodable review is a bad request")
	}
}

func TestMirror_MutateIgnoresOthers(t *testing.T) {
	if r := testMirror.mutate(&admissionv1.AdmissionRequest{Kind: metav1.GroupVersionKind{Kind: "Deployment"}}); !r.Allowed || r.Patch != nil {
		t.Fatal("only pods are mutated")
	}
	if r := testMirror.mutate(&admissionv1.AdmissionRequest{Kind: metav1.GroupVersionKind{Kind: "Pod"}, Object: runtime.RawExtension{Raw: []byte("{")}}); !r.Allowed || r.Patch != nil {
		t.Fatal("a pod that does not parse is allowed unchanged")
	}
	off := NewValidator(Config{})
	rec := httptest.NewRecorder()
	off.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mutate", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("with the mirror off /mutate is not served: %d", rec.Code)
	}
}

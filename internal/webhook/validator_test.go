package webhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func container(name, image string, limits bool, probe bool) corev1.Container {
	c := corev1.Container{Name: name, Image: image}
	if limits {
		c.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("500m")}
	}
	if probe {
		c.ReadinessProbe = &corev1.Probe{}
	}
	return c
}

func deploymentRequest(t *testing.T, cs ...corev1.Container) *admissionv1.AdmissionRequest {
	t.Helper()
	d := appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: cs}}}}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionv1.AdmissionRequest{UID: types.UID("u1"), Kind: metav1.GroupVersionKind{Kind: "Deployment"}, Object: runtime.RawExtension{Raw: raw}}
}

func TestValidate(t *testing.T) {
	strict := Config{RequireLimits: true, RequireReadiness: true, BlockedImages: []string{"docker.io/untrusted/"}}
	noMem := container("app", "app:1.2", true, true)
	delete(noMem.Resources.Limits, corev1.ResourceMemory)
	noCPU := container("app", "app:1.2", true, true)
	delete(noCPU.Resources.Limits, corev1.ResourceCPU)
	cases := []struct {
		name    string
		cfg     Config
		c       corev1.Container
		allowed bool
		want    string
	}{
		{"good", strict, container("app", "app:1.2", true, true), true, ""},
		{"no limits", strict, container("app", "app:1.2", false, true), false, "no resource limits"},
		{"no memory limit", strict, noMem, false, "no memory limit"},
		{"no cpu limit warns", strict, noCPU, true, "no CPU limit"},
		{"no readiness", strict, container("app", "app:1.2", true, false), false, "no readiness probe"},
		{"blocked image", strict, container("app", "docker.io/untrusted/x:1", true, true), false, "blocked image prefix"},
		{"latest warns", Config{}, container("app", "app:latest", false, false), true, "untagged"},
		{"registry port without a tag warns", Config{}, container("app", "registry:5000/app", false, false), true, "untagged"},
		{"registry port with a tag", Config{}, container("app", "registry:5000/app:2.0", false, false), true, ""},
		{"digest", Config{}, container("app", "app@sha256:abc", false, false), true, ""},
	}
	for _, c := range cases {
		resp := validate(deploymentRequest(t, c.c), c.cfg)
		text := strings.Join(resp.Warnings, " ")
		if resp.Result != nil {
			text += resp.Result.Message
		}
		if resp.Allowed != c.allowed || (c.want == "" && text != "") || (c.want != "" && !strings.Contains(text, c.want)) {
			t.Errorf("%s: allowed=%v text=%q", c.name, resp.Allowed, text)
		}
	}
	other := &admissionv1.AdmissionRequest{Kind: metav1.GroupVersionKind{Kind: "Pod"}}
	if !validate(other, strict).Allowed {
		t.Fatal("kinds other than Deployment and StatefulSet pass")
	}
	broken := &admissionv1.AdmissionRequest{Kind: metav1.GroupVersionKind{Kind: "StatefulSet"}, Object: runtime.RawExtension{Raw: []byte("{")}}
	if !validate(broken, strict).Allowed {
		t.Fatal("an object that does not parse is not blocked")
	}
	sts := appsv1.StatefulSet{Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{container("db", "db:1", false, true)}}}}}
	raw, _ := json.Marshal(sts)
	if validate(&admissionv1.AdmissionRequest{Kind: metav1.GroupVersionKind{Kind: "StatefulSet"}, Object: runtime.RawExtension{Raw: raw}}, strict).Allowed {
		t.Fatal("a StatefulSet without limits is denied")
	}
}

func TestServeValidate(t *testing.T) {
	v := NewValidator(Config{RequireLimits: true})
	if v.srv.Addr != ":8443" {
		t.Fatalf("default port: %s", v.srv.Addr)
	}
	review := admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: deploymentRequest(t, container("app", "app:1", false, false))}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	post := func(b []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		v.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/validate", bytes.NewReader(b)))
		return rec
	}
	rec := post(body)
	var out admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Response == nil || out.Response.Allowed || out.Response.UID != "u1" {
		t.Fatalf("response %s (%v)", rec.Body.String(), err)
	}
	if post([]byte("not json")).Code != http.StatusBadRequest {
		t.Fatal("an undecodable body is a bad request")
	}
	empty, _ := json.Marshal(admissionv1.AdmissionReview{TypeMeta: review.TypeMeta})
	if post(empty).Code != http.StatusBadRequest {
		t.Fatal("a review without a request is a bad request")
	}
	h := httptest.NewRecorder()
	v.srv.Handler.ServeHTTP(h, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if h.Body.String() != "ok" {
		t.Fatal("healthz")
	}
}

package kube

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDedupKey(t *testing.T) {
	key := dedupKey("default", "my-pod-abc", "CrashLoopBackOff")
	expected := "default/my-pod-abc/CrashLoopBackOff"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestOwnerName_WithController(t *testing.T) {
	isCtrl := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-pod-abc",
			OwnerReferences: []metav1.OwnerReference{
				{
					Kind:       "ReplicaSet",
					Name:       "my-deploy-xyz",
					Controller: &isCtrl,
				},
			},
		},
	}
	name := ownerName(pod)
	if name != "replicaset/my-deploy-xyz" {
		t.Errorf("expected 'replicaset/my-deploy-xyz', got %q", name)
	}
}

func TestOwnerName_NoController(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone-pod"},
	}
	name := ownerName(pod)
	if name != "pod/standalone-pod" {
		t.Errorf("expected 'pod/standalone-pod', got %q", name)
	}
}

func TestImageOf(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx:1.25"},
				{Name: "sidecar", Image: "envoy:latest"},
			},
		},
	}

	if img := imageOf(pod, "app"); img != "nginx:1.25" {
		t.Errorf("expected 'nginx:1.25', got %q", img)
	}
	if img := imageOf(pod, "sidecar"); img != "envoy:latest" {
		t.Errorf("expected 'envoy:latest', got %q", img)
	}
	if img := imageOf(pod, "missing"); img != "" {
		t.Errorf("expected empty string for missing container, got %q", img)
	}
}

func TestHasAnnotation(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"auto-agent.io/disable": "true",
			},
		},
	}

	if !hasAnnotation(pod, "auto-agent.io/disable") {
		t.Error("expected annotation to be found")
	}
	if hasAnnotation(pod, "nonexistent") {
		t.Error("expected annotation not to be found")
	}
	if hasAnnotation(pod, "") {
		t.Error("expected empty key to return false")
	}
}

func TestHasAnnotation_NilAnnotations(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{}}
	if hasAnnotation(pod, "any-key") {
		t.Error("expected false for nil annotations")
	}
}

func TestIsCriticalPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "system-node-critical",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "monitoring"},
				Spec:       corev1.PodSpec{PriorityClassName: "system-node-critical"},
			},
			expected: true,
		},
		{
			name: "kube-system pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"},
				Spec:       corev1.PodSpec{},
			},
			expected: true,
		},
		{
			name: "regular pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
				Spec:       corev1.PodSpec{},
			},
			expected: false,
		},
		{
			name: "critical in name",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "prod"},
				Spec:       corev1.PodSpec{PriorityClassName: "my-critical-class"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCriticalPod(tt.pod); got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestIsStatefulSetPod(t *testing.T) {
	isCtrl := true
	stsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "StatefulSet", Name: "my-sts", Controller: &isCtrl},
			},
		},
	}
	if !isStatefulSetPod(stsPod) {
		t.Error("expected StatefulSet pod to be detected")
	}

	rsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "my-rs", Controller: &isCtrl},
			},
		},
	}
	if isStatefulSetPod(rsPod) {
		t.Error("expected ReplicaSet pod not to be detected as StatefulSet")
	}
}

func TestParseDuration(t *testing.T) {
	if d := parseDuration("5m", "1m"); d != 5*time.Minute {
		t.Errorf("expected 5m, got %v", d)
	}
	if d := parseDuration("invalid", "2m"); d != 2*time.Minute {
		t.Errorf("expected 2m fallback, got %v", d)
	}
	if d := parseDuration("", "30s"); d != 30*time.Second {
		t.Errorf("expected 30s fallback, got %v", d)
	}
}

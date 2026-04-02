package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPressureChanged(t *testing.T) {
	tests := []struct {
		name     string
		oldConds []corev1.NodeCondition
		newConds []corev1.NodeCondition
		expected bool
	}{
		{
			name:     "no change - both healthy",
			oldConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}},
			newConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}},
			expected: false,
		},
		{
			name:     "memory pressure appeared",
			oldConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}},
			newConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}},
			expected: true,
		},
		{
			name:     "disk pressure appeared",
			oldConds: []corev1.NodeCondition{{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse}},
			newConds: []corev1.NodeCondition{{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue}},
			expected: true,
		},
		{
			name:     "pressure resolved",
			oldConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}},
			newConds: []corev1.NodeCondition{{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status:     corev1.NodeStatus{Conditions: tt.oldConds},
			}
			newNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
				Status:     corev1.NodeStatus{Conditions: tt.newConds},
			}
			got := pressureChanged(oldNode, newNode)
			if got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

func TestConditionStatus(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			},
		},
	}

	if s := conditionStatus(node, corev1.NodeReady); s != corev1.ConditionTrue {
		t.Errorf("expected ConditionTrue, got %v", s)
	}
	if s := conditionStatus(node, corev1.NodeMemoryPressure); s != corev1.ConditionFalse {
		t.Errorf("expected ConditionFalse, got %v", s)
	}
	if s := conditionStatus(node, corev1.NodeDiskPressure); s != corev1.ConditionUnknown {
		t.Errorf("expected ConditionUnknown for missing condition, got %v", s)
	}
}

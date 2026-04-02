package kube

import (
	"testing"
	"time"
)

func TestInCooldown(t *testing.T) {
	tests := []struct {
		name     string
		annos    map[string]string
		key      string
		cooldown time.Duration
		expected bool
	}{
		{
			name:     "nil annotations",
			annos:    nil,
			key:      annoLastScaleUp,
			cooldown: 2 * time.Minute,
			expected: false,
		},
		{
			name:     "missing key",
			annos:    map[string]string{},
			key:      annoLastScaleUp,
			cooldown: 2 * time.Minute,
			expected: false,
		},
		{
			name:     "recent scale up",
			annos:    map[string]string{annoLastScaleUp: time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)},
			key:      annoLastScaleUp,
			cooldown: 2 * time.Minute,
			expected: true,
		},
		{
			name:     "old scale up",
			annos:    map[string]string{annoLastScaleUp: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
			key:      annoLastScaleUp,
			cooldown: 2 * time.Minute,
			expected: false,
		},
		{
			name:     "invalid timestamp",
			annos:    map[string]string{annoLastScaleUp: "not-a-date"},
			key:      annoLastScaleUp,
			cooldown: 2 * time.Minute,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inCooldown(tt.annos, tt.key, tt.cooldown)
			if got != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, got)
			}
		})
	}
}

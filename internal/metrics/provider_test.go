package metrics

import "testing"

func TestSanitizeLabelValue(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"my-deploy", "my-deploy"},
		{"api.service", "api.service"},
		{"safe_name", "safe_name"},
		{`"})+OR+vector(1)`, "____OR_vector_1_"},
		{"normal-123", "normal-123"},
		{"has spaces", "has_spaces"},
		{"has;semi", "has_semi"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeLabelValue(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeLabelValue(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	r := New()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "OpenAI key",
			input:    "Here is the key: sk-abcdef1234567890abcdef1234567890 please keep it safe",
			expected: "Here is the key: [REDACTED] please keep it safe",
		},
		{
			name:     "Anthropic key",
			input:    "Claude token: sk-ant-api03-abcdef12345678901234567890123456789012",
			expected: "Claude token: [REDACTED]",
		},
		{
			name:     "Gemini key",
			input:    "GCP key AIzaSyD12345678901234567890123456789012 in logs",
			expected: "GCP key [REDACTED] in logs",
		},
		{
			name:     "GitHub token",
			input:    "ghp_123456789012345678901234567890123456",
			expected: "[REDACTED]",
		},
		{
			name:     "Telegram bot token",
			input:    "Bot token: 123456789:ABCDEF1234567890abcdef1234567890123",
			expected: "Bot token: [REDACTED]",
		},
		{
			name: "Private Key block",
			input: `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA0Y1u3v...
supersecretkeydata
-----END RSA PRIVATE KEY-----`,
			expected: "[REDACTED]",
		},
		{
			name:     "Generic secret assignment",
			input:    "config: api_key = \"supersecretkey123456789\"",
			expected: "config: api_key: [REDACTED]",
		},
		{
			name:     "Clean text unmodified",
			input:    "All unit tests passed in 4.2 seconds.",
			expected: "All unit tests passed in 4.2 seconds.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.Redact(tt.input)
			if !strings.Contains(got, "[REDACTED]") && strings.Contains(tt.expected, "[REDACTED]") {
				t.Errorf("expected [REDACTED] in output, got %q", got)
			}
			if tt.name == "Clean text unmodified" && got != tt.expected {
				t.Errorf("expected clean text unmodified, got %q", got)
			}
		})
	}
}

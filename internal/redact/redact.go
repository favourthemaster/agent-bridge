package redact

import (
	"regexp"
)

type Redactor struct {
	patterns []*regexp.Regexp
}

func New() *Redactor {
	patterns := []*regexp.Regexp{
		// Private keys (RSA, EC, OPENSSH, etc.)
		regexp.MustCompile(`-----BEGIN [A-Z0-9_\- ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9_\- ]*PRIVATE KEY-----`),
		// OpenAI API Keys
		regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{20,}\b`),
		// Anthropic API Keys
		regexp.MustCompile(`\bsk-ant-[a-zA-Z0-9_-]{20,}\b`),
		// Google Cloud / Gemini API Keys
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
		// GitHub Personal Access Tokens
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{36,}\b`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{50,}\b`),
		// Telegram Bot Tokens
		regexp.MustCompile(`\b[0-9]{8,10}:[a-zA-Z0-9_-]{35}\b`),
		// Slack Tokens
		regexp.MustCompile(`\bxox[baprs]-[0-9a-zA-Z]{10,48}\b`),
		// Bearer JWT Tokens
		regexp.MustCompile(`(?i)\bBearer\s+[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]{10,}\b`),
		// Generic key/secret assignments
		regexp.MustCompile(`(?i)\b(api_key|secret_key|private_key|auth_token|access_token|password)\s*[:=]\s*["']?([A-Za-z0-9_\-!@#$%^&*+=]{12,})["']?`),
	}
	return &Redactor{patterns: patterns}
}

// Redact replaces sensitive tokens and private keys with [REDACTED].
func (r *Redactor) Redact(text string) string {
	res := text
	for _, p := range r.patterns {
		if p.NumSubexp() >= 2 {
			// Pattern has submatches (e.g. key: secret) -> preserve key name, redact value
			res = p.ReplaceAllString(res, "$1: [REDACTED]")
		} else {
			res = p.ReplaceAllString(res, "[REDACTED]")
		}
	}
	return res
}

package formatter

import (
	"fmt"
	"strings"
)

// FormatThinkingTrace formats internal agent reasoning into a collapsible Telegram blockquote.
func FormatThinkingTrace(thinking string) string {
	trimmed := strings.TrimSpace(thinking)
	if trimmed == "" {
		return ""
	}

	if len(trimmed) > 2000 {
		trimmed = trimmed[:2000] + "..."
	}

	escaped := EscapeHTML(trimmed)
	return fmt.Sprintf("<blockquote expandable>💭 <b>Thinking Process</b>\n<i>%s</i></blockquote>\n\n", escaped)
}

// FormatStderrTail extracts and formats the last N lines of stderr in a monospace block.
func FormatStderrTail(stderr string, maxLines int) string {
	trimmed := strings.TrimSpace(stderr)
	if trimmed == "" {
		return ""
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	escaped := EscapeHTML(strings.Join(lines, "\n"))
	return fmt.Sprintf("<pre><code>%s</code></pre>", escaped)
}

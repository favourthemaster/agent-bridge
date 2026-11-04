package formatter

import (
	"strings"
	"unicode/utf8"
)

const MaxTelegramMessageLength = 4096

// SplitMessage splits an HTML formatted string into chunks <= maxLen (default 4096 runes).
// It avoids splitting in the middle of code blocks or open tags.
func SplitMessage(html string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = MaxTelegramMessageLength
	}

	if utf8.RuneCountInString(html) <= maxLen {
		return []string{html}
	}

	var chunks []string
	remaining := html

	inPre := false
	preLang := ""

	for utf8.RuneCountInString(remaining) > maxLen {
		// Find split boundary within maxLen runes
		splitIdx := findSplitIndex(remaining, maxLen)

		chunk := remaining[:splitIdx]
		remaining = remaining[splitIdx:]

		// Track tag states in this chunk
		preOpenCount := strings.Count(chunk, "<pre>") + strings.Count(chunk, "<pre ")
		preCloseCount := strings.Count(chunk, "</pre>")

		if inPre {
			if preCloseCount > preOpenCount {
				inPre = false
			}
		} else {
			if preOpenCount > preCloseCount {
				inPre = true
			}
		}

		// If split while inPre is active, close it for this chunk and reopen for next
		if inPre {
			chunk = chunk + "</code></pre>"
			if preLang != "" {
				remaining = "<pre><code class=\"language-" + preLang + "\">" + remaining
			} else {
				remaining = "<pre><code>" + remaining
			}
		}

		chunks = append(chunks, chunk)
	}

	if len(remaining) > 0 {
		chunks = append(chunks, remaining)
	}

	return chunks
}

func findSplitIndex(s string, maxRunes int) int {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return len(s)
	}

	// Search backwards from maxRunes for paragraph break (\n\n)
	sub := string(runes[:maxRunes])
	if idx := strings.LastIndex(sub, "\n\n"); idx > maxRunes/2 {
		return idx + 2
	}

	// Search backwards for newline (\n)
	if idx := strings.LastIndex(sub, "\n"); idx > maxRunes/3 {
		return idx + 1
	}

	// Search backwards for space
	if idx := strings.LastIndex(sub, " "); idx > maxRunes/4 {
		return idx + 1
	}

	// Hard split at maxRunes
	return len(string(runes[:maxRunes]))
}

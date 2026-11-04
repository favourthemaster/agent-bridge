package formatter

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	headerRegex   = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	boldRegex     = regexp.MustCompile(`\*\*(.+?)\*\*|__(.+?)__`)
	italicRegex   = regexp.MustCompile(`(?:\*([^\*\n]+?)\*|_([^\_\n]+?)_)`)
	strikeRegex   = regexp.MustCompile(`~~(.+?)~~`)
	linkRegex     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	listDashRegex = regexp.MustCompile(`(?m)^(\s*)[-\*]\s+(.+)$`)
	listNumRegex  = regexp.MustCompile(`(?m)^(\s*)(\d+)\.\s+(.+)$`)
)

// EscapeHTML escapes special characters for Telegram HTML parse mode.
func EscapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// StripHTML removes basic HTML tags for fallback to plain text.
func StripHTML(s string) string {
	tagRegex := regexp.MustCompile(`<[^>]*>`)
	text := tagRegex.ReplaceAllString(s, "")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&amp;", "&")
	return text
}

// MarkdownToHTML converts markdown text to Telegram HTML format.
func MarkdownToHTML(md string) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}

	lines := strings.Split(md, "\n")
	var out strings.Builder

	inCodeBlock := false
	codeBlockLang := ""
	var codeLines []string

	inQuoteBlock := false
	var quoteLines []string

	flushQuote := func() {
		if len(quoteLines) > 0 {
			quotedText := strings.Join(quoteLines, "\n")
			out.WriteString(fmt.Sprintf("<blockquote>%s</blockquote>\n", processInlineMarkdown(quotedText)))
			quoteLines = nil
			inQuoteBlock = false
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Check for code block boundary
		if strings.HasPrefix(trimmed, "```") {
			flushQuote()
			if inCodeBlock {
				// Close code block
				content := EscapeHTML(strings.Join(codeLines, "\n"))
				if codeBlockLang != "" {
					out.WriteString(fmt.Sprintf("<pre><code class=\"language-%s\">%s</code></pre>\n", codeBlockLang, content))
				} else {
					out.WriteString(fmt.Sprintf("<pre><code>%s</code></pre>\n", content))
				}
				inCodeBlock = false
				codeLines = nil
				codeBlockLang = ""
			} else {
				// Open code block
				inCodeBlock = true
				codeBlockLang = strings.TrimPrefix(trimmed, "```")
				codeBlockLang = strings.TrimSpace(codeBlockLang)
				codeLines = nil
			}
			continue
		}

		if inCodeBlock {
			codeLines = append(codeLines, line)
			continue
		}

		// Check for blockquote
		if strings.HasPrefix(trimmed, ">") {
			inQuoteBlock = true
			quoteContent := strings.TrimPrefix(trimmed, ">")
			quoteLines = append(quoteLines, strings.TrimPrefix(quoteContent, " "))
			continue
		} else if inQuoteBlock {
			flushQuote()
		}

		// Check for table row (convert to monospace block)
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			// Skip markdown table separators like |---|---|
			if strings.Contains(trimmed, "---") {
				continue
			}
			out.WriteString(fmt.Sprintf("<code>%s</code>\n", EscapeHTML(line)))
			continue
		}

		// Process headers
		if headerMatch := headerRegex.FindStringSubmatch(line); len(headerMatch) > 1 {
			out.WriteString(fmt.Sprintf("<b>%s</b>\n", processInlineMarkdown(headerMatch[1])))
			continue
		}

		// Process unordered lists
		if listMatch := listDashRegex.FindStringSubmatch(line); len(listMatch) > 2 {
			indent := listMatch[1]
			content := listMatch[2]
			out.WriteString(fmt.Sprintf("%s• %s\n", indent, processInlineMarkdown(content)))
			continue
		}

		// Process ordered lists
		if numMatch := listNumRegex.FindStringSubmatch(line); len(numMatch) > 3 {
			indent := numMatch[1]
			num := numMatch[2]
			content := numMatch[3]
			out.WriteString(fmt.Sprintf("%s%s. %s\n", indent, num, processInlineMarkdown(content)))
			continue
		}

		// Normal line with inline markdown
		out.WriteString(processInlineMarkdown(line) + "\n")
	}

	flushQuote()

	// If unclosed code block, close it safely
	if inCodeBlock {
		content := EscapeHTML(strings.Join(codeLines, "\n"))
		out.WriteString(fmt.Sprintf("<pre><code>%s</code></pre>\n", content))
	}

	return strings.TrimRight(out.String(), "\n")
}

// processInlineMarkdown handles bold, italic, strikethrough, inline code, and links.
func processInlineMarkdown(line string) string {
	// First protect inline code blocks by extracting them
	inlineCodePattern := regexp.MustCompile("`([^`]+)`")
	var codePlaceholders []string
	line = inlineCodePattern.ReplaceAllStringFunc(line, func(match string) string {
		inner := strings.Trim(match, "`")
		placeholder := fmt.Sprintf("\uFFF0INLINECODE%d\uFFF0", len(codePlaceholders))
		codePlaceholders = append(codePlaceholders, fmt.Sprintf("<code>%s</code>", EscapeHTML(inner)))
		return placeholder
	})

	// Escape remaining raw HTML characters
	line = EscapeHTML(line)

	// Bold: **text** or __text__
	line = boldRegex.ReplaceAllStringFunc(line, func(match string) string {
		sub := boldRegex.FindStringSubmatch(match)
		content := sub[1]
		if content == "" {
			content = sub[2]
		}
		return fmt.Sprintf("<b>%s</b>", content)
	})

	// Italic: *text* or _text_
	line = italicRegex.ReplaceAllStringFunc(line, func(match string) string {
		sub := italicRegex.FindStringSubmatch(match)
		content := sub[1]
		if content == "" {
			content = sub[2]
		}
		return fmt.Sprintf("<i>%s</i>", content)
	})

	// Strikethrough: ~~text~~
	line = strikeRegex.ReplaceAllString(line, "<s>$1</s>")

	// Links: [text](url)
	line = linkRegex.ReplaceAllString(line, `<a href="$2">$1</a>`)

	// Restore inline code blocks
	for i, codeHTML := range codePlaceholders {
		placeholder := fmt.Sprintf("\uFFF0INLINECODE%d\uFFF0", i)
		line = strings.ReplaceAll(line, placeholder, codeHTML)
	}

	return line
}

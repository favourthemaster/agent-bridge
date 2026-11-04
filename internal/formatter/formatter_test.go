package formatter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarkdownToHTMLFixtures(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{
			name:  "Header, bold, italic, strike, link",
			input: "# Main Title\nThis is **bold** and *italic* and ~~deleted~~ with [Google](https://google.com).",
			contains: []string{
				"<b>Main Title</b>",
				"<b>bold</b>",
				"<i>italic</i>",
				"<s>deleted</s>",
				`<a href="https://google.com">Google</a>`,
			},
		},
		{
			name:  "Code block with < and &",
			input: "```go\nfunc check(a < b && c > d) {}\n```",
			contains: []string{
				`<pre><code class="language-go">func check(a &lt; b &amp;&amp; c &gt; d) {}</code></pre>`,
			},
		},
		{
			name:  "Inline code with < and &",
			input: "Check `a < 5 && b > 2` in code.",
			contains: []string{
				`Check <code>a &lt; 5 &amp;&amp; b &gt; 2</code> in code.`,
			},
		},
		{
			name:  "Nested and bulleted lists",
			input: "- Item 1\n  * Subitem 1.1\n1. Numbered 1\n2. Numbered 2",
			contains: []string{
				"• Item 1",
				"  • Subitem 1.1",
				"1. Numbered 1",
				"2. Numbered 2",
			},
		},
		{
			name:  "Unclosed code block recovery",
			input: "```python\nprint('hello world')",
			contains: []string{
				"<pre><code>print('hello world')",
				"</code></pre>",
			},
		},
		{
			name:  "Blockquote",
			input: "> Warning: this is important.\n> Verify before running.",
			contains: []string{
				"<blockquote>Warning: this is important.\nVerify before running.</blockquote>",
			},
		},
		{
			name:  "Markdown table into monospace",
			input: "| Name | Status |\n|---|---|\n| test | ok |",
			contains: []string{
				"<code>| Name | Status |</code>",
				"<code>| test | ok |</code>",
			},
		},
		{
			name:  "Emoji and RTL text",
			input: "🚀 Launching! مرحبا بالعالم",
			contains: []string{
				"🚀 Launching! مرحبا بالعالم",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToHTML(tt.input)
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("expected output to contain %q, but got:\n%s", c, got)
				}
			}
		})
	}
}

func TestSplitMessageCodeBlockIntegrity(t *testing.T) {
	// Generate a long code block exceeding 100 runes
	codeContent := strings.Repeat("x = x + 1\ny = y + 2\n", 20)
	input := "<pre><code>" + codeContent + "</code></pre>"

	maxLen := 80
	chunks := SplitMessage(input, maxLen)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		if utf8.RuneCountInString(c) > maxLen+30 { // allowing tag closure overhead
			t.Errorf("chunk %d exceeded limit: len=%d", i, utf8.RuneCountInString(c))
		}
		// Every chunk that starts a pre must close it
		if strings.Contains(c, "<pre>") && !strings.Contains(c, "</pre>") {
			t.Errorf("chunk %d has unclosed <pre>: %q", i, c)
		}
	}
}

func TestStripHTMLFallback(t *testing.T) {
	input := "<b>Error:</b> <code>bad &lt;syntax&gt;</code> in <a href=\"http://test\">link</a>."
	got := StripHTML(input)
	expected := "Error: bad <syntax> in link."
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestFormatThinkingTrace(t *testing.T) {
	thinking := "Step 1: check files.\nStep 2: fix bug <&>."
	got := FormatThinkingTrace(thinking)
	if !strings.Contains(got, "<blockquote expandable>") || !strings.Contains(got, "Thinking Process") {
		t.Errorf("unexpected thinking trace: %s", got)
	}
	if !strings.Contains(got, "&lt;&amp;&gt;") {
		t.Errorf("expected escaped HTML characters in thinking trace, got %s", got)
	}
}

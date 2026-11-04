package parser

import (
	"regexp"
	"strings"
)

var (
	// CSI: \x1b[ ... [a-zA-Z] (including ?, >, = modifiers)
	ansiCsiRegex = regexp.MustCompile(`\x1b\[[0-9;?><=]*[a-zA-Z]`)
	// OSC: \x1b] ... (\x07 | \x1b\\)
	ansiOscRegex = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	// Other VT escapes
	ansiGenericRegex = regexp.MustCompile(`\x1b[()][0-9A-Za-z]|\x1b[=>]`)
)

// StripVT removes all ANSI/VT100 escape sequences, screen switches, and cursor movement codes.
func StripVT(input string) string {
	s := ansiCsiRegex.ReplaceAllString(input, "")
	s = ansiOscRegex.ReplaceAllString(s, "")
	s = ansiGenericRegex.ReplaceAllString(s, "")

	// Normalize carriage returns
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

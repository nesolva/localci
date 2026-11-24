package report

import (
	"regexp"
	"strings"
	"unicode"
)

// MaxReason is the longest infrastructure reason a run keeps, in characters.
const MaxReason = 200

// Printable makes text that came from a run's report (written by the code under test) safe for a terminal:
// control characters, escape sequences included, and the bidirectional overrides that reorder a line become
// U+FFFD. Tabs and line breaks become spaces.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r), isBidi(r):
			return '�'
		}
		return r
	}, s)
}

func isBidi(r rune) bool {
	return r >= '‪' && r <= '‮' || r >= '⁦' && r <= '⁩'
}

// escapes matches terminal escape sequences whole, so that stripping the control characters does not leave their
// parameters behind ("\x1b[31m" must not become "[31m"): CSI (7-bit and 8-bit introducer), OSC up to its
// terminator, and the two-character forms.
var escapes = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x9b[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?|\x1b[@-Z\\-_]`)

// Reason turns text that may carry anything (a tenant flow's error, a service's last output line) into the
// one-line reason a run keeps for an infrastructure failure: escape sequences removed, the first line only, the
// C0 and C1 control characters and the bidirectional overrides dropped (a tab stays), at most MaxReason
// characters. It returns "" when nothing printable is left.
func Reason(s string) string { return Line(s, MaxReason) }

// Line is Reason with the cap as a parameter: one printable line of at most max characters.
func Line(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = escapes.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.Map(func(r rune) rune {
		if r != '\t' && unicode.IsControl(r) || isBidi(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

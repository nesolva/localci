package config

import (
	"fmt"
	"strings"
)

// Error points at the exact place a value is wrong: the file and line for the
// config file, or the env var / flag name for the other layers.
type Error struct {
	File  string // config file path, or "env LOCALCI_X" / "flag --x"
	Line  int    // 0 when not from a file
	Field string // dotted key
	Msg   string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.File)
	if e.Line > 0 {
		fmt.Fprintf(&b, ":%d", e.Line)
	}
	if e.Field != "" {
		b.WriteString(": " + e.Field)
	}
	b.WriteString(": " + e.Msg)
	return b.String()
}

// Errors collects every problem found so one run reports them all.
type Errors []*Error

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// suggest returns the candidate closest to name (edit distance <= 2), or "".
func suggest(name string, candidates []string) string {
	best, bestD := "", 3
	for _, c := range candidates {
		if d := editDistance(name, c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

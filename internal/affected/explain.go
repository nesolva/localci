package affected

import (
	"fmt"
	"strings"
)

// Explain is what `localci run --affected --explain` prints: why each test, or why all.
func Explain(s Selection) string {
	var b strings.Builder
	switch {
	case len(s.Changed) == 0:
		b.WriteString("changed: nothing\n")
	default:
		fmt.Fprintf(&b, "changed: %d %s\n", len(s.Changed), plural(len(s.Changed), "file"))
		for i, f := range s.Changed {
			if i == 10 {
				fmt.Fprintf(&b, "  … %d more\n", len(s.Changed)-i)
				break
			}
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}
	if s.All {
		fmt.Fprintf(&b, "all %d tests: %s\n", s.Total, s.Reason)
		return b.String()
	}
	fmt.Fprintf(&b, "%d of %d tests affected\n", len(s.Items), s.Total)
	for i, it := range s.Items {
		if i == 10 {
			fmt.Fprintf(&b, "  … %d more\n", len(s.Items)-i)
			break
		}
		fmt.Fprintf(&b, "  %s  because %s\n", it.Name, strings.Join(limit(it.Because, 3), ", "))
	}
	return b.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func limit(l []string, n int) []string {
	if len(l) <= n {
		return l
	}
	return append(append([]string{}, l[:n]...), fmt.Sprintf("+%d more", len(l)-n))
}

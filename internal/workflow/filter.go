package workflow

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// trigger is the configuration of one event under `on:`.
type trigger struct {
	branches, branchesIgnore []string
	paths, pathsIgnore       []string
	types                    []string // activity types; empty means the event's defaults
}

// headTypes are the pull request activities a new or pushed head is: GitHub's
// default types for pull_request, and what localci plans checks for.
var headTypes = []string{"opened", "synchronize", "reopened"}

// readTriggers turns the value of `on:` (a string, a list or a map) into the
// triggers it declares, keyed by event name.
func readTriggers(n *yaml.Node) map[string]*trigger {
	out := map[string]*trigger{}
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil {
		return out
	}
	switch n.Kind {
	case yaml.ScalarNode:
		out[n.Value] = &trigger{}
	case yaml.SequenceNode:
		for _, it := range n.Content {
			out[it.Value] = &trigger{}
		}
	case yaml.MappingNode:
		keys, vals := mapPairs(n)
		for _, k := range keys {
			t := &trigger{}
			_, cfg := mapPairs(vals[k])
			t.branches = strList(cfg["branches"])
			t.branchesIgnore = strList(cfg["branches-ignore"])
			t.paths = strList(cfg["paths"])
			t.pathsIgnore = strList(cfg["paths-ignore"])
			t.types = strList(cfg["types"])
			out[k] = t
		}
	}
	return out
}

func strList(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, it := range n.Content {
		out = append(out, it.Value)
	}
	return out
}

// skipReason says why the trigger does not fire for ev; "" means it does.
func (t *trigger) skipReason(ev Event) string {
	if (ev.Name == "pull_request" || ev.Name == "pull_request_target") && len(t.types) > 0 && !anyHeadType(t.types) {
		return fmt.Sprintf("types [%s] include none of %s", strings.Join(t.types, ", "), strings.Join(headTypes, ", "))
	}
	if ev.Branch != "" {
		if len(t.branches) > 0 && !matchAny(t.branches, ev.Branch) {
			return fmt.Sprintf("branch %s does not match branches", ev.Branch)
		}
		if len(t.branchesIgnore) > 0 && filterSelects(t.branchesIgnore, ev.Branch, false) {
			return fmt.Sprintf("branch %s matches branches-ignore", ev.Branch)
		}
	}
	if ev.Files == nil { // unknown change set: cannot filter by path
		return ""
	}
	if len(t.paths) > 0 && !anyFileMatches(t.paths, ev.Files) {
		return fmt.Sprintf("no changed file matches paths (%d changed)", len(ev.Files))
	}
	if len(t.pathsIgnore) > 0 && len(ev.Files) > 0 && allFilesMatch(t.pathsIgnore, ev.Files) {
		return fmt.Sprintf("all %d changed files match paths-ignore", len(ev.Files))
	}
	return ""
}

// anyHeadType reports whether a types list includes an activity that is a new
// or pushed pull request head.
func anyHeadType(types []string) bool {
	for _, t := range types {
		for _, h := range headTypes {
			if strings.EqualFold(t, h) {
				return true
			}
		}
	}
	return false
}

// invalidPatterns names the filter patterns that do not compile, one message
// each, so the planned checks say their filter could not be applied.
func (t *trigger) invalidPatterns() []string {
	var out []string
	for _, list := range [][]string{t.branches, t.branchesIgnore, t.paths, t.pathsIgnore} {
		for _, p := range list {
			if _, err := globCompile(strings.TrimPrefix(p, "!")); err != nil {
				out = append(out, fmt.Sprintf("invalid filter pattern %q, treated in favour of running", p))
			}
		}
	}
	return out
}

// matchAny reports whether s is selected by an ordered inclusion filter list
// (branches or paths): "!" patterns exclude what an earlier one included.
func matchAny(patterns []string, s string) bool { return filterSelects(patterns, s, true) }

// pathSelected applies an inclusion filter list (paths) to one file.
func pathSelected(patterns []string, file string) bool { return filterSelects(patterns, file, true) }

// filterSelects applies a filter list in order: a later "!" pattern excludes
// what an earlier one included, and a later plain pattern includes it again,
// like GitHub does. include says whether the list selects what runs
// (branches, paths) or what is skipped (*-ignore): a pattern that does not
// compile resolves the way that plans the check, never the way that drops it.
func filterSelects(patterns []string, s string, include bool) bool {
	selected := false
	for _, p := range patterns {
		pat, isNeg := strings.CutPrefix(p, "!")
		m, ok := globMatch(pat, s)
		if !ok {
			m = include != isNeg
		}
		if m {
			selected = !isNeg
		}
	}
	return selected
}

type globEntry struct {
	re  *regexp.Regexp
	err error
}

var globCache sync.Map // pattern -> globEntry

func globCompile(pattern string) (*regexp.Regexp, error) {
	v, ok := globCache.Load(pattern)
	if !ok {
		re, err := regexp.Compile(globRegexp(pattern))
		v = globEntry{re, err}
		globCache.Store(pattern, v)
	}
	e := v.(globEntry)
	return e.re, e.err
}

// globMatch matches s with GitHub's filter pattern syntax: "*" is any
// characters except "/", "**" any characters including "/" (also inside a
// segment, as in "**.go"), "?" zero or one of the preceding character, "+" one
// or more of it, "[...]" a character class. "**/" may also match nothing, so
// "**/*.js" selects a top-level a.js (the wider reading: a check is planned
// rather than dropped). ok is false when the pattern does not compile.
func globMatch(pattern, s string) (match, ok bool) {
	re, err := globCompile(pattern)
	if err != nil {
		return false, false
	}
	return re.MatchString(s), true
}

func globRegexp(p string) string {
	var sb strings.Builder
	sb.WriteString("^")
	atom := false // the last thing written can take a ? or + quantifier
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++
				sb.WriteString("(?:.*/)?")
			} else {
				sb.WriteString(".*")
			}
			atom = false
		case c == '*':
			sb.WriteString("[^/]*")
			atom = false
		case (c == '?' || c == '+') && atom:
			sb.WriteByte(c)
			atom = false
		case c == '[':
			end := strings.IndexByte(p[i+1:], ']')
			if end < 0 {
				sb.WriteString(`\[`)
				atom = true
				break
			}
			class := p[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			sb.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += end + 1
			atom = true
		case c == '\\' && i+1 < len(p):
			_, n := utf8.DecodeRuneInString(p[i+1:])
			sb.WriteString(regexp.QuoteMeta(p[i+1 : i+1+n]))
			i += n
			atom = true
		default:
			// Quote the whole UTF-8 sequence: string(byte) would be the
			// Latin-1 rune and mangle non-ASCII text.
			_, n := utf8.DecodeRuneInString(p[i:])
			sb.WriteString(regexp.QuoteMeta(p[i : i+n]))
			i += n - 1
			atom = true
		}
	}
	sb.WriteString("$")
	return sb.String()
}

func anyFileMatches(patterns, files []string) bool {
	for _, f := range files {
		if pathSelected(patterns, f) {
			return true
		}
	}
	return false
}

func allFilesMatch(patterns, files []string) bool {
	for _, f := range files {
		if !filterSelects(patterns, f, false) {
			return false
		}
	}
	return true
}

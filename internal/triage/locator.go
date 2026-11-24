package triage

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nesolva/localci/internal/report"
)

// locator is the first call of the chain a failure says it looked for.
type locator struct {
	Text   string // the chain as the message prints it
	Method string // getByRole, getByLabel, getByText, getByPlaceholder, getByTitle, getByAltText, getByTestId
	Role   string // getByRole
	Name   string // accessible name, text, label or test id; "" when the call has none
	Strict int    // >1: how many elements the locator resolved to
}

// near is what the aria excerpt says about a locator that found nothing (or too much).
type near struct {
	found bool // the excerpt holds something the locator was probably meant to find
	lines []string
	fix   *Fix
}

var (
	locatorLine = regexp.MustCompile(`(?m)^\s*Locator:\s+(\S.*)$`)
	waitingFor  = regexp.MustCompile(`(?m)waiting for\s+((?:getBy\w+|locator)\(.*)$`)
	strictRe    = regexp.MustCompile(`strict mode violation:\s+(.+?)\s+resolved to (\d+) elements`)
)

// locate finds the locator a failure was looking for and what the page holds in its place. loc is nil when the
// failure names no locator, or one that was found (the failure is then about its state, not its selector); n is
// nil when there is no usable aria excerpt to compare with.
func locate(f report.Failure) (loc *locator, n *near) {
	msg := f.Message
	if msg == "" {
		return nil, nil
	}
	var text string
	strict := 0
	if m := strictRe.FindStringSubmatch(msg); m != nil {
		text = m[1]
		strict, _ = strconv.Atoi(m[2])
	} else if m := locatorLine.FindStringSubmatch(msg); m != nil {
		text = m[1]
	} else if m := waitingFor.FindStringSubmatch(msg); m != nil {
		text = m[1]
	}
	if text == "" {
		return nil, nil
	}
	l := parseLocator(text)
	if l == nil {
		return nil, nil
	}
	l.Strict = strict
	// A locator that resolved to an element failed for another reason (its text, its state); only a locator that
	// found nothing, or too much, is a selector problem.
	notFound := strings.Contains(msg, "element(s) not found") ||
		(strings.Contains(msg, "waiting for") && !strings.Contains(msg, "locator resolved to"))
	if strict < 2 && !notFound {
		return nil, nil
	}
	if strict > 1 {
		return l, &near{found: true, lines: []string{
			fmt.Sprintf("locator %s matches %d elements (strict mode)", clip(l.Text, 90), strict),
			"it needs to say which one; no automatic fix",
		}}
	}
	if f.Aria == nil {
		return l, nil
	}
	return l, nearMatch(l, parseAria(*f.Aria))
}

type ariaEntry struct{ Role, Name string }

var ariaLine = regexp.MustCompile(`^\s*-\s+([a-zA-Z]+)(?:\s+"((?:[^"\\]|\\.)*)")?`)
var ariaText = regexp.MustCompile(`^\s*-\s+(?:text|paragraph|heading|cell|listitem|term|definition|code|emphasis|strong|time|generic):\s+(.+)$`)

func parseAria(s string) []ariaEntry {
	var out []ariaEntry
	for _, line := range strings.Split(s, "\n") {
		if m := ariaLine.FindStringSubmatch(line); m != nil && m[2] != "" {
			out = append(out, ariaEntry{m[1], unescape(m[2])})
		} else if m := ariaText.FindStringSubmatch(line); m != nil {
			t := strings.TrimSpace(m[1])
			out = append(out, ariaEntry{"text", strings.Trim(t, `"`)})
		}
	}
	return out
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// fixMethods are the calls whose first literal is the text a patch may replace.
var fixMethods = map[string]bool{"getByRole": true, "getByLabel": true, "getByText": true, "getByPlaceholder": true,
	"getByTitle": true, "getByAltText": true}

const maxDistance = 3

func nearMatch(l *locator, entries []ariaEntry) *near {
	if l.Method == "getByTestId" {
		// An aria tree carries no test ids; a name that holds the id is the nearest thing to the same one.
		for _, e := range entries {
			if l.Name != "" && strings.Contains(strings.ToLower(e.Name), strings.ToLower(l.Name)) {
				return &near{found: true, lines: []string{
					fmt.Sprintf("test id %q is not on the page; %s %q holds it", l.Name, e.Role, clip(e.Name, 60)),
					"no automatic fix for a test id",
				}}
			}
		}
		return &near{}
	}
	if l.Name == "" {
		return &near{}
	}
	want := strings.ToLower(l.Name)
	type cand struct {
		e    ariaEntry
		dist int
	}
	var cands []cand
	sameRole := 0
	for _, e := range entries {
		if l.Method == "getByRole" && e.Role != l.Role {
			continue
		}
		sameRole++
		d := distance(want, strings.ToLower(e.Name))
		if d == 0 {
			// The element is there under the same name: the selector is right and something else is wrong.
			return &near{}
		}
		if d <= maxDistance {
			cands = append(cands, cand{e, d})
		}
	}
	reason := ""
	switch {
	case len(cands) > 0:
		best := cands[0]
		ties := map[string]bool{}
		for _, c := range cands {
			if c.dist < best.dist {
				best, ties = c, map[string]bool{}
			}
			if c.dist == best.dist {
				ties[c.e.Name] = true
			}
		}
		if len(ties) > 1 {
			var names []string
			for n := range ties {
				names = append(names, strconv.Quote(clip(n, 30)))
			}
			sort.Strings(names)
			return &near{found: true, lines: []string{
				fmt.Sprintf("locator %s matches nothing; %d names are as near", clip(l.Text, 90), len(ties)),
				"candidates: " + strings.Join(names, ", "),
			}}
		}
		cands = []cand{best}
		reason = fmt.Sprintf("edit distance %d", best.dist)
	case l.Method == "getByRole" && sameRole == 1:
		// One element with that role is on the page: the likeliest reading is a renamed control.
		for _, e := range entries {
			if e.Role == l.Role {
				cands = []cand{{e, -1}}
			}
		}
		reason = "the only " + l.Role + " on the page"
	default:
		return &near{}
	}
	c := cands[0]
	v := &near{found: true, lines: []string{
		fmt.Sprintf("locator %s matches nothing", clip(l.Text, 90)),
		fmt.Sprintf("the page has %s %q (%s)", c.e.Role, clip(c.e.Name, 60), reason),
	}}
	if fixMethods[l.Method] && l.Name != "" {
		v.fix = &Fix{Method: l.Method, Role: l.Role, From: l.Name, To: c.e.Name}
	}
	return v
}

// distance is the Levenshtein distance between two strings, in characters.
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return max(len(ra), len(rb))
	}
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// jsValue is a literal of a locator call: a string, an object of literals, or anything else.
type jsValue struct {
	str   string
	isStr bool
	obj   map[string]jsValue
}

// jsParser reads the literals of a call the way Playwright prints it (getByRole('button', { name: 'Pay' })).
type jsParser struct {
	s string
	i int
	// Spans (byte offsets) of the literals a patch replaces: the last string read, the first argument of the
	// call, and the value of a `name` key.
	lastS, lastE int
	arg0S, arg0E int
	nameS, nameE int
}

func (p *jsParser) ws() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *jsParser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *jsParser) ident() string {
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i]
}

// stringLit reads a quoted literal at the cursor and returns its value.
func (p *jsParser) stringLit() (string, bool) {
	q := p.peek()
	if q != '\'' && q != '"' && q != '`' {
		return "", false
	}
	start := p.i
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == q:
			p.i++
			p.lastS, p.lastE = start, p.i
			return b.String(), true
		case c == '\\' && p.i+1 < len(p.s):
			p.i++
			switch e := p.s[p.i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'u':
				if p.i+4 < len(p.s) {
					if n, err := strconv.ParseUint(p.s[p.i+1:p.i+5], 16, 32); err == nil {
						b.WriteRune(rune(n))
						p.i += 4
						break
					}
				}
				b.WriteByte(e)
			default:
				b.WriteByte(e)
			}
			p.i++
		case c == '$' && q == '`' && p.i+1 < len(p.s) && p.s[p.i+1] == '{':
			return "", false // an interpolation: not a literal
		default:
			r, n := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r)
			p.i += n
		}
	}
	return "", false
}

func (p *jsParser) value() (jsValue, bool) {
	p.ws()
	if s, ok := p.stringLit(); ok {
		return jsValue{str: s, isStr: true}, true
	}
	switch p.peek() {
	case '{':
		p.i++
		obj := map[string]jsValue{}
		for {
			p.ws()
			if p.peek() == '}' {
				p.i++
				return jsValue{obj: obj}, true
			}
			key := p.ident()
			if key == "" {
				if k, ok := p.stringLit(); ok {
					key = k
				} else {
					return jsValue{}, false
				}
			}
			p.ws()
			if p.peek() != ':' {
				return jsValue{}, false
			}
			p.i++
			v, ok := p.value()
			if !ok {
				return jsValue{}, false
			}
			obj[key] = v
			if key == "name" && v.isStr {
				p.nameS, p.nameE = p.lastS, p.lastE
			}
			p.ws()
			if p.peek() == ',' {
				p.i++
			}
		}
	case '/':
		// A regular expression: skip to its closing slash and its flags.
		p.i++
		for p.i < len(p.s) && p.s[p.i] != '/' {
			if p.s[p.i] == '\\' {
				p.i++
			}
			p.i++
		}
		p.i++
		p.ident()
		return jsValue{}, true
	}
	// true, false, numbers, null.
	if id := p.ident(); id != "" {
		return jsValue{}, true
	}
	return jsValue{}, false
}

// call reads `name(args…)` at the cursor.
func (p *jsParser) call() (name string, args []jsValue, ok bool) {
	p.ws()
	name = p.ident()
	p.ws()
	if name == "" || p.peek() != '(' {
		return "", nil, false
	}
	p.i++
	for {
		p.ws()
		if p.peek() == ')' {
			p.i++
			return name, args, true
		}
		v, ok := p.value()
		if !ok {
			return "", nil, false
		}
		if len(args) == 0 && v.isStr {
			p.arg0S, p.arg0E = p.lastS, p.lastE
		}
		args = append(args, v)
		p.ws()
		if p.peek() == ',' {
			p.i++
		}
	}
}

// parseLocator reads the first call of a printed locator chain.
func parseLocator(text string) *locator {
	p := &jsParser{s: strings.TrimSpace(text)}
	name, args, ok := p.call()
	if !ok {
		return nil
	}
	// The rest of the chain, if any, is part of the text but not of what the fix looks at.
	l := &locator{Text: strings.TrimSpace(text), Method: name}
	switch name {
	case "getByRole":
		if len(args) > 0 && args[0].isStr {
			l.Role = args[0].str
		}
		if len(args) > 1 && args[1].obj != nil {
			if n, ok := args[1].obj["name"]; ok && n.isStr {
				l.Name = n.str
			}
		}
		if l.Role == "" {
			return nil
		}
	case "getByLabel", "getByText", "getByPlaceholder", "getByTitle", "getByAltText", "getByTestId":
		if len(args) > 0 && args[0].isStr {
			l.Name = args[0].str
		}
	default:
		return nil
	}
	return l
}

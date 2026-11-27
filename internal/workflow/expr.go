package workflow

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// scope holds what expressions can be resolved against at plan time.
type scope struct {
	matrix map[string]string
	// typed keeps the YAML type of matrix values (bool, number, null) for
	// expressions; keys missing here fall back to the string in matrix.
	typed map[string]val
	env   map[string]string
	event Event
	sha   string
}

type valKind uint8

const (
	kindString valKind = iota
	kindBool
	kindNumber
	kindNull
	kindObject
)

// val is an expression value: a string, boolean, number, null or object. s is
// the text form (what ${{ }} prints; for a number, the text the author wrote;
// for an object, its compact JSON).
type val struct {
	kind valKind
	s    string
	b    bool
	n    float64
	obj  map[string]val // kindObject: the entries `matrix.a.b` reads
}

func strVal(s string) val { return val{s: s} }
func boolVal(b bool) val  { return val{kind: kindBool, b: b} }
func nullVal() val        { return val{kind: kindNull} }
func numVal(n float64, text string) val {
	return val{kind: kindNumber, n: n, s: text}
}

// truthy follows GitHub: false, 0, NaN, "" and null are falsy, anything else
// (including the string "false" or "0") is truthy.
func (v val) truthy() bool {
	switch v.kind {
	case kindBool:
		return v.b
	case kindNull:
		return false
	case kindNumber:
		return v.n != 0 && !math.IsNaN(v.n)
	}
	return v.s != ""
}

func (v val) String() string {
	if v.kind == kindBool {
		if v.b {
			return "true"
		}
		return "false"
	}
	return v.s
}

// number coerces a value to a number the way GitHub's == does when the types
// differ: null is 0, booleans 1/0, strings are parsed ("" is 0, junk is NaN).
func (v val) number() float64 {
	switch v.kind {
	case kindNumber:
		return v.n
	case kindNull:
		return 0
	case kindBool:
		if v.b {
			return 1
		}
		return 0
	}
	s := strings.TrimSpace(v.s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// looseEqual is GitHub's == : same types compare directly (strings ignoring
// case), different types compare as numbers.
func looseEqual(l, r val) bool {
	if l.kind == r.kind {
		switch l.kind {
		case kindNull:
			return true
		case kindBool:
			return l.b == r.b
		case kindNumber:
			return l.n == r.n
		case kindObject:
			return false // GitHub compares objects by reference
		}
		return strings.EqualFold(l.s, r.s)
	}
	return l.number() == r.number() // NaN is never equal
}

// typedVal reads a YAML matrix scalar with its YAML type.
func typedVal(n *yaml.Node) val {
	return typedValIn(n, map[*yaml.Node]*val{})
}

// typedValIn reads each mapping once, so aliases cannot make it expand: seen
// holds the mappings read so far, and nil for those still being read. An alias
// back into one of those (`&x {a: *x}`) reads as null instead of recursing for
// ever; a reused anchor (`&a1 {l: *a0, r: *a0}`, level after level) shares the
// object read the first time instead of reading it 2^n times. The objects are
// only read afterwards, so sharing them is safe.
func typedValIn(n *yaml.Node, seen map[*yaml.Node]*val) val {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n != nil && n.Kind == yaml.MappingNode {
		if v, ok := seen[n]; ok {
			if v == nil {
				return nullVal()
			}
			return *v
		}
		seen[n] = nil
		keys, vals := mapPairs(n)
		obj := make(map[string]val, len(keys))
		for _, k := range keys {
			obj[k] = typedValIn(vals[k], seen)
		}
		v := val{kind: kindObject, s: scalar(n), obj: obj}
		seen[n] = &v
		return v
	}
	if n == nil || n.Kind != yaml.ScalarNode {
		return strVal(scalar(n))
	}
	switch n.Tag {
	case "!!null":
		return nullVal()
	case "!!bool":
		var b bool
		if n.Decode(&b) == nil {
			return boolVal(b)
		}
	case "!!int", "!!float":
		var f float64
		if n.Decode(&f) == nil {
			return numVal(f, n.Value)
		}
	}
	return strVal(n.Value)
}

// unsupportedError is returned for anything the evaluator cannot resolve at
// plan time (secrets, step outputs, functions, other contexts).
type unsupportedError struct{ what string }

func (e *unsupportedError) Error() string { return e.what }

func unsupported(format string, a ...any) error {
	return &unsupportedError{fmt.Sprintf(format, a...)}
}

var exprRe = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// subst replaces every ${{ expr }} in s. Expressions it cannot resolve are
// left as they are and reported.
func (sc *scope) subst(s string) (string, []string) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	var bad []string
	out := exprRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := strings.TrimSpace(m[3 : len(m)-2])
		v, _, err := sc.evaluate(inner, false)
		if err != nil {
			bad = append(bad, fmt.Sprintf("${{ %s }}: %v", inner, err))
			return m
		}
		return v.String()
	})
	return out, bad
}

// condition evaluates an if: expression. always reports whether the
// expression names always() and holds, i.e. the step runs after a failure.
func (sc *scope) condition(expr string) (run, always bool, err error) {
	e := strings.TrimSpace(expr)
	if strings.HasPrefix(e, "${{") && strings.HasSuffix(e, "}}") && strings.Count(e, "${{") == 1 {
		e = strings.TrimSpace(e[3 : len(e)-2])
	}
	v, sawAlways, err := sc.evaluate(e, true)
	if err != nil {
		return true, false, err
	}
	run = v.truthy()
	return run, run && sawAlways, nil
}

// evaluate evaluates one expression. In status mode the status functions resolve
// as for a job that is still passing: success() and always() are true,
// failure() and cancelled() are false.
func (sc *scope) evaluate(expr string, status bool) (val, bool, error) {
	p := &parser{src: expr, sc: sc, status: status}
	v, err := p.parseOr()
	if err != nil {
		return val{}, false, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return val{}, false, unsupported("unexpected %q", p.src[p.pos:])
	}
	return v, p.sawAlways, nil
}

type parser struct {
	src       string
	pos       int
	sc        *scope
	status    bool
	sawAlways bool
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n') {
		p.pos++
	}
}

func (p *parser) accept(tok string) bool {
	p.skipSpace()
	if strings.HasPrefix(p.src[p.pos:], tok) {
		p.pos += len(tok)
		return true
	}
	return false
}

func (p *parser) parseOr() (val, error) {
	l, err := p.parseAnd()
	if err != nil {
		return val{}, err
	}
	for p.accept("||") {
		r, err := p.parseAnd()
		if err != nil {
			return val{}, err
		}
		if !l.truthy() {
			l = r
		}
	}
	return l, nil
}

func (p *parser) parseAnd() (val, error) {
	l, err := p.parseEq()
	if err != nil {
		return val{}, err
	}
	for p.accept("&&") {
		r, err := p.parseEq()
		if err != nil {
			return val{}, err
		}
		if l.truthy() {
			l = r
		}
	}
	return l, nil
}

func (p *parser) parseEq() (val, error) {
	l, err := p.parseUnary()
	if err != nil {
		return val{}, err
	}
	for {
		var neg bool
		switch {
		case p.accept("=="):
		case p.accept("!="):
			neg = true
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return val{}, err
		}
		l = boolVal(looseEqual(l, r) != neg)
	}
}

func (p *parser) parseUnary() (val, error) {
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '!' && !strings.HasPrefix(p.src[p.pos:], "!=") {
		p.pos++
		v, err := p.parseUnary()
		if err != nil {
			return val{}, err
		}
		return boolVal(!v.truthy()), nil
	}
	return p.parsePrimary()
}

func isIdent(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (p *parser) parsePrimary() (val, error) {
	p.skipSpace()
	if p.pos >= len(p.src) {
		return val{}, unsupported("empty expression")
	}
	switch c := p.src[p.pos]; {
	case c == '(':
		p.pos++
		v, err := p.parseOr()
		if err != nil {
			return val{}, err
		}
		if !p.accept(")") {
			return val{}, unsupported("missing )")
		}
		return v, nil
	case c == '\'':
		p.pos++
		var sb strings.Builder
		for p.pos < len(p.src) {
			if p.src[p.pos] == '\'' {
				if p.pos+1 < len(p.src) && p.src[p.pos+1] == '\'' {
					sb.WriteByte('\'')
					p.pos += 2
					continue
				}
				p.pos++
				return strVal(sb.String()), nil
			}
			sb.WriteByte(p.src[p.pos])
			p.pos++
		}
		return val{}, unsupported("unterminated string")
	}
	start := p.pos
	for p.pos < len(p.src) && isIdent(p.src[p.pos]) {
		p.pos++
	}
	word := p.src[start:p.pos]
	if word == "" {
		return val{}, unsupported("unexpected %q", p.src[p.pos:])
	}
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		return p.call(word)
	}
	if p.pos < len(p.src) && p.src[p.pos] == '[' {
		return val{}, unsupported("index access on %s", word)
	}
	return p.ident(word)
}

// call resolves the status functions; everything else is unsupported.
func (p *parser) call(name string) (val, error) {
	p.pos++ // (
	if !p.accept(")") {
		return val{}, unsupported("function %s()", name)
	}
	if !p.status {
		return val{}, unsupported("function %s() outside if:", name)
	}
	switch name {
	case "always":
		p.sawAlways = true
		return boolVal(true), nil
	case "success":
		return boolVal(true), nil
	case "failure", "cancelled":
		return boolVal(false), nil
	}
	return val{}, unsupported("function %s()", name)
}

func (p *parser) ident(word string) (val, error) {
	switch word {
	case "true":
		return boolVal(true), nil
	case "false":
		return boolVal(false), nil
	case "null":
		return nullVal(), nil
	case "github.event_name":
		return strVal(p.sc.event.Name), nil
	case "github.sha":
		if p.sc.sha == "" {
			return val{}, unsupported("github.sha")
		}
		return strVal(p.sc.sha), nil
	case "github.base_ref":
		if p.sc.event.Name != "pull_request" {
			return strVal(""), nil
		}
		if p.sc.event.Branch == "" {
			return val{}, unsupported("github.base_ref")
		}
		return strVal(p.sc.event.Branch), nil
	}
	if k, ok := strings.CutPrefix(word, "matrix."); ok {
		if t, ok := p.sc.typed[k]; ok {
			return t, nil
		}
		// `matrix.config.name`: walk through object values. Like on GitHub, a
		// missing key (or a key under a value that is not an object) is null.
		if head, rest, nested := strings.Cut(k, "."); nested {
			cur, ok := p.sc.typed[head]
			if !ok {
				return nullVal(), nil
			}
			for _, part := range strings.Split(rest, ".") {
				if cur.kind != kindObject {
					return nullVal(), nil
				}
				if cur, ok = cur.obj[part]; !ok {
					return nullVal(), nil
				}
			}
			return cur, nil
		}
		v, ok := p.sc.matrix[k]
		if !ok {
			return nullVal(), nil // an absent matrix key is null, like on GitHub
		}
		return strVal(v), nil
	}
	if k, ok := strings.CutPrefix(word, "env."); ok {
		if v, ok := p.sc.env[k]; ok {
			return strVal(v), nil
		}
		return val{}, unsupported("env.%s is not set in the workflow or job", k)
	}
	if c := word[:strings.IndexAny(word+".", ".")]; c != "" {
		switch c {
		case "secrets", "vars", "steps", "needs", "runner", "inputs", "job", "github", "strategy":
			return val{}, unsupported("%s", word)
		}
	}
	if f, err := strconv.ParseFloat(word, 64); err == nil {
		return numVal(f, word), nil
	}
	return val{}, unsupported("%s", word)
}

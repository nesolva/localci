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
	// needs is needs.<id>.result of the jobs the job needs. needSkipped is
	// set for a job's if: when one of them is skipped: success() is then
	// false.
	needs       map[string]string
	needSkipped bool
}

type valKind uint8

const (
	kindString valKind = iota
	kindBool
	kindNumber
	kindNull
)

// val is an expression value: a string, boolean, number or null. s is the
// text form (what ${{ }} prints; for a number, the text the author wrote).
//
// why, when set, means the value is not known at plan time (a secret, a step
// output) and says what made it so. known is then its truth when the
// expression decides it anyway: `false && x` is falsy whatever x is.
type val struct {
	kind  valKind
	s     string
	b     bool
	n     float64
	why   string
	known truth
}

// truth is a truth value that may be unknown at plan time.
type truth uint8

const (
	truthUnknown truth = iota
	truthFalse
	truthTrue
)

func unknownVal(why string) val { return val{why: why} }

// truth is truthy() for a known value, and known for an unknown one.
func (v val) truth() truth {
	switch {
	case v.why != "":
		return v.known
	case v.truthy():
		return truthTrue
	}
	return truthFalse
}

// and is GitHub's &&: the left value when it is falsy, else the right one.
func and(l, r val) val {
	switch l.truth() {
	case truthFalse:
		return l
	case truthTrue:
		return r
	}
	if r.truth() == truthFalse {
		return val{why: l.why, known: truthFalse}
	}
	return unknownVal(l.why)
}

// or is GitHub's ||: the left value when it is truthy, else the right one.
func or(l, r val) val {
	switch l.truth() {
	case truthTrue:
		return l
	case truthFalse:
		return r
	}
	if r.truth() == truthTrue {
		return val{why: l.why, known: truthTrue}
	}
	return unknownVal(l.why)
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
		}
		return strings.EqualFold(l.s, r.s)
	}
	return l.number() == r.number() // NaN is never equal
}

// typedVal reads a YAML matrix scalar with its YAML type.
func typedVal(n *yaml.Node) val {
	if n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
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

// unsupportedError is returned for an expression the evaluator cannot resolve
// at plan time: one it cannot parse, or whose value depends on what is only
// known when the job runs (secrets, step outputs, other contexts).
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
		if err == nil && v.why != "" {
			err = unsupported("%s", v.why)
		}
		if err != nil {
			bad = append(bad, fmt.Sprintf("${{ %s }}: %v", inner, err))
			return m
		}
		return v.String()
	})
	return out, bad
}

// trimExpr removes the ${{ }} around a whole if: expression.
func trimExpr(expr string) string {
	e := strings.TrimSpace(expr)
	if strings.HasPrefix(e, "${{") && strings.HasSuffix(e, "}}") && strings.Count(e, "${{") == 1 {
		e = strings.TrimSpace(e[3 : len(e)-2])
	}
	return e
}

// usesStatus reports whether an if: calls success(), failure(), cancelled()
// or always(), wherever in the expression.
func usesStatus(expr string) bool {
	if expr == "" {
		return false
	}
	p := &parser{src: trimExpr(expr), sc: &scope{}, status: true}
	p.parseOr()
	return p.sawStatus
}

// condition evaluates an if: expression. always reports whether the
// expression names always() and holds, i.e. the step runs after a failure.
func (sc *scope) condition(expr string) (run, always bool, err error) {
	v, sawAlways, err := sc.evaluate(trimExpr(expr), true)
	if err != nil {
		return true, false, err
	}
	switch v.truth() {
	case truthUnknown:
		return true, false, unsupported("%s", v.why)
	case truthFalse:
		return false, false, nil
	}
	return true, sawAlways, nil
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
	sawStatus bool
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
		l = or(l, r)
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
		l = and(l, r)
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
		switch {
		case l.why != "":
			l = unknownVal(l.why)
		case r.why != "":
			l = unknownVal(r.why)
		default:
			l = boolVal(looseEqual(l, r) != neg)
		}
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
		switch v.truth() {
		case truthUnknown:
			return unknownVal(v.why), nil
		case truthTrue:
			return boolVal(false), nil
		}
		return boolVal(true), nil
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
	word := p.word()
	if word == "" {
		return val{}, unsupported("unexpected %q", p.src[p.pos:])
	}
	var v val
	if p.pos < len(p.src) && p.src[p.pos] == '(' {
		var err error
		if v, err = p.call(word); err != nil {
			return val{}, err
		}
	} else {
		v = p.ident(word)
	}
	// An index or a property of the result (labels[0].name, fromJSON(x).a)
	// is only known when the job runs.
	for p.pos < len(p.src) && (p.src[p.pos] == '[' || p.src[p.pos] == '.') {
		if p.src[p.pos] == '.' {
			p.word()
		} else {
			p.pos++
			if _, err := p.parseOr(); err != nil {
				return val{}, err
			}
			if !p.accept("]") {
				return val{}, unsupported("missing ]")
			}
		}
		if v.why == "" {
			v = unknownVal("index access on " + word)
		}
	}
	return v, nil
}

// word reads an identifier or number, with the .* of a filter
// (labels.*.name).
func (p *parser) word() string {
	start := p.pos
	for p.pos < len(p.src) && (isIdent(p.src[p.pos]) || p.src[p.pos] == '*' && p.pos > start && p.src[p.pos-1] == '.') {
		p.pos++
	}
	return p.src[start:p.pos]
}

// call resolves the status functions and the string tests; any other function
// is only known when the job runs.
func (p *parser) call(name string) (val, error) {
	p.pos++ // (
	var args []val
	if !p.accept(")") {
		for {
			a, err := p.parseOr()
			if err != nil {
				return val{}, err
			}
			args = append(args, a)
			if p.accept(")") {
				break
			}
			if !p.accept(",") {
				return val{}, unsupported("missing ) after the arguments of %s()", name)
			}
		}
	}
	switch name {
	case "always", "success", "failure", "cancelled":
		if len(args) > 0 {
			return val{}, unsupported("%s() takes no arguments", name)
		}
		p.sawStatus = true
		if !p.status {
			return unknownVal("function " + name + "() outside if:"), nil
		}
		switch name {
		case "always":
			p.sawAlways = true
			return boolVal(true), nil
		case "success":
			return boolVal(!p.sc.needSkipped), nil
		}
		return boolVal(false), nil
	case "startsWith", "endsWith", "contains":
		if len(args) != 2 {
			return val{}, unsupported("%s() takes two arguments", name)
		}
		for _, a := range args {
			if a.why != "" {
				return unknownVal(a.why), nil
			}
		}
		// GitHub casts both to strings and ignores case. contains() on an
		// array needs fromJSON or a filter, which are unknown here.
		s, sub := strings.ToLower(args[0].String()), strings.ToLower(args[1].String())
		switch name {
		case "startsWith":
			return boolVal(strings.HasPrefix(s, sub)), nil
		case "endsWith":
			return boolVal(strings.HasSuffix(s, sub)), nil
		}
		return boolVal(strings.Contains(s, sub)), nil
	}
	return unknownVal("function " + name + "()"), nil
}

// ref is github.ref or github.ref_name: refs/pull/<n>/merge on a pull request,
// refs/heads/<branch> on a push. Without the pull request number (localci
// plan, a working tree) a condition still sees a pull request ref, which no
// branch or tag test matches; in text the number is not guessed.
func (p *parser) ref(word string) val {
	ev := p.sc.event
	var ref string
	switch {
	case ev.Name == "pull_request" && (ev.PR > 0 || p.status):
		ref = fmt.Sprintf("refs/pull/%d/merge", ev.PR)
	case ev.Name == "push" && ev.Branch != "":
		ref = "refs/heads/" + ev.Branch
	default:
		return unknownVal(word)
	}
	if word == "github.ref_name" {
		ref = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/pull/"), "refs/heads/")
	}
	return strVal(ref)
}

func (p *parser) ident(word string) val {
	switch word {
	case "true":
		return boolVal(true)
	case "false":
		return boolVal(false)
	case "null":
		return nullVal()
	case "github.event_name":
		return strVal(p.sc.event.Name)
	case "github.sha":
		if p.sc.sha == "" {
			return unknownVal("github.sha")
		}
		return strVal(p.sc.sha)
	case "github.base_ref":
		if p.sc.event.Name != "pull_request" {
			return strVal("")
		}
		if p.sc.event.Branch == "" {
			return unknownVal("github.base_ref")
		}
		return strVal(p.sc.event.Branch)
	case "github.head_ref":
		if p.sc.event.Name != "pull_request" {
			return strVal("")
		}
		if p.sc.event.HeadBranch == "" {
			return unknownVal("github.head_ref")
		}
		return strVal(p.sc.event.HeadBranch)
	case "github.ref", "github.ref_name":
		return p.ref(word)
	case "github.ref_type":
		return strVal("branch") // a pull request or a branch push; tags are not planned
	case "github.repository", "github.repository_owner":
		if p.sc.event.Repo == "" {
			return unknownVal(word)
		}
		if word == "github.repository_owner" {
			owner, _, _ := strings.Cut(p.sc.event.Repo, "/")
			return strVal(owner)
		}
		return strVal(p.sc.event.Repo)
	}
	if k, ok := strings.CutPrefix(word, "matrix."); ok {
		if t, ok := p.sc.typed[k]; ok {
			return t
		}
		v, ok := p.sc.matrix[k]
		if !ok {
			return nullVal() // an absent matrix key is null, like on GitHub
		}
		return strVal(v)
	}
	if k, ok := strings.CutPrefix(word, "env."); ok {
		if v, ok := p.sc.env[k]; ok {
			return strVal(v)
		}
		return unknownVal("env." + k + " is not set in the workflow or job")
	}
	if rest, ok := strings.CutPrefix(word, "needs."); ok {
		if id, field, _ := strings.Cut(rest, "."); field == "result" && p.sc.needs[id] != "" {
			return strVal(p.sc.needs[id])
		}
		return unknownVal(word)
	}
	if f, err := strconv.ParseFloat(word, 64); err == nil {
		return numVal(f, word)
	}
	// secrets, vars, steps, needs, runner, inputs, job, strategy, the rest of
	// github, and names GitHub does not know.
	return unknownVal(word)
}

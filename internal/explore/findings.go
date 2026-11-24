package explore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/store"
)

// What a findings file may hold. The agent that writes it drives an app nobody reviewed, so every bound is a
// decision about untrusted input.
const (
	MaxFindings  = 100
	MaxTitle     = 200
	MaxGoalText  = MaxGoalLen
	MaxSteps     = 30
	MaxStep      = 300
	MaxLine      = 256 << 10 // a longer line is not read
	MaxFile      = 8 << 20   // the findings file is read up to here
	MaxDraft     = 64 << 10  // a draft test is cut here
	draftDirName = "drafts"
)

// Severities are what a finding may say; anything else is "low".
var Severities = []string{"high", "medium", "low"}

// rawFinding is one line of the findings file.
type rawFinding struct {
	Goal       string `json:"goal"`
	Severity   string `json:"severity"`
	Title      string `json:"title"`
	Steps      any    `json:"steps"`
	Screenshot string `json:"screenshot"`
	Test       string `json:"test"`
}

// Parsed is what a findings file made.
type Parsed struct {
	Findings []store.Finding
	Skipped  int // lines that were not a usable finding
}

// ReadFindings reads the NDJSON file at path (it may be missing or end in a line the agent was killed in the middle
// of) and returns the findings it holds as stored data: text printable and capped, the screenshot a path inside
// outDir or nothing, a draft test saved under outDir/drafts and named by its path. run and suite are set on each.
func ReadFindings(path, outDir, run, suite string) (Parsed, error) {
	var p Parsed
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, MaxFile), 64<<10)
	for {
		line, tooLong, err := readLine(r)
		if len(line) > 0 || tooLong {
			if len(p.Findings) >= MaxFindings {
				p.Skipped++
			} else if fd, ok := p.parseLine(line, tooLong, outDir); ok {
				fd.Run, fd.Suite = run, suite
				p.Findings = append(p.Findings, fd)
			} else {
				p.Skipped++
			}
		}
		if err != nil {
			break
		}
	}
	for i := range p.Findings {
		fd := &p.Findings[i]
		if fd.Test == "" {
			continue
		}
		fd.Test = saveDraft(outDir, i+1, fd.Test)
	}
	return p, nil
}

// readLine reads one line; a line longer than MaxLine is skipped to its end and reported as too long.
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, isPrefix, e := r.ReadLine()
		if e != nil {
			return line, tooLong, e
		}
		if !tooLong {
			line = append(line, chunk...)
			if len(line) > MaxLine {
				line, tooLong = nil, true
			}
		}
		if !isPrefix {
			return line, tooLong, nil
		}
	}
}

func (p *Parsed) parseLine(line []byte, tooLong bool, outDir string) (store.Finding, bool) {
	var rf rawFinding
	if tooLong || json.Unmarshal(line, &rf) != nil {
		return store.Finding{}, false
	}
	title := Text(rf.Title, MaxTitle)
	if title == "" {
		return store.Finding{}, false
	}
	fd := store.Finding{Title: title, Goal: Text(rf.Goal, MaxGoalText), Severity: severity(rf.Severity), Steps: steps(rf.Steps)}
	fd.Screenshot = ConfinedImage(outDir, rf.Screenshot)
	if t := strings.TrimSpace(rf.Test); t != "" {
		fd.Test = rf.Test // saved after the list is complete, so the draft's number is the finding's
	}
	return fd, true
}

func severity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, v := range Severities {
		if s == v {
			return v
		}
	}
	return "low"
}

// Text makes one line of untrusted text printable (no escape sequence survives, controls and bidirectional
// overrides are gone) and at most max characters.
func Text(s string, max int) string {
	return report.Line(s, max)
}

func steps(v any) []string {
	var raw []string
	switch t := v.(type) {
	case string:
		raw = strings.Split(t, "\n")
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				raw = append(raw, s)
			}
		}
	}
	out := []string{}
	for _, s := range raw {
		if s = Text(s, MaxStep); s != "" {
			out = append(out, s)
		}
		if len(out) == MaxSteps {
			break
		}
	}
	return out
}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true}

// ConfinedImage returns the absolute path of the image file p names when, with symlinks and ".." resolved, it is a
// regular image file inside dir; otherwise "". A relative p is taken relative to dir.
func ConfinedImage(dir, p string) string {
	if p == "" || strings.ContainsRune(p, 0) || !imageExts[strings.ToLower(filepath.Ext(p))] {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	if rel, err := filepath.Rel(root, real); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if fi, err := os.Stat(real); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return real
}

// saveDraft writes a draft test under dir/drafts and returns its path ("" when it cannot be written). The text is
// never run by localci and never committed; control characters other than line breaks and tabs are dropped.
func saveDraft(dir string, n int, text string) string {
	text = strings.ToValidUTF8(text, "")
	if len(text) > MaxDraft {
		text = text[:MaxDraft]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	text = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	d := filepath.Join(dir, draftDirName)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return ""
	}
	// The agent can write to the output directory: refuse a drafts directory or a file that is a link.
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		return ""
	}
	path := filepath.Join(d, fmt.Sprintf("finding-%02d.spec.ts", n))
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return ""
	}
	_, werr := f.WriteString(text + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return ""
	}
	return path
}

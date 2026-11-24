package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Patch is a one-line change to a test file, as a unified diff that `git apply` takes from the repository root.
type Patch struct {
	File string // relative to the repository root, slash-separated
	Diff string
	Line int // 1-based line the change is on
}

// safePath is what a patch header can carry without quoting.
var safePath = regexp.MustCompile(`^[A-Za-z0-9._@+/-]+$`)

// BuildPatch writes the change that replaces fix.From with fix.To in the locator the failure points at. The
// failure is untrusted: location names the file, and the file must resolve inside workspace and be one of the
// suite's test files (their paths relative to the test directory or the repository, any suffix of the real path),
// or there is no patch and the reason says why.
func BuildPatch(workspace, location string, testFiles []string, fix Fix) (*Patch, string) {
	file, line, ok := splitLocation(location)
	if !ok {
		return nil, "no patch: the failure has no file:line location"
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, "no patch: the run's workspace is gone"
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(root, file)
	}
	real, err := filepath.EvalSymlinks(file)
	if err != nil {
		return nil, "no patch: the file the failure names does not exist in the workspace"
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, "no patch: the file the failure names is outside the workspace"
	}
	rel = filepath.ToSlash(rel)
	if !isTestFile(rel, testFiles) {
		return nil, "no patch: the file the failure names is not one of the suite's test files"
	}
	if !tracked(context.Background(), root, rel) {
		return nil, "no patch: the file the failure names is not tracked by git"
	}
	if !safePath.MatchString(rel) {
		return nil, "no patch: the test file's path has characters a patch cannot carry"
	}
	content, err := os.ReadFile(real)
	if err != nil {
		return nil, "no patch: cannot read the test file"
	}
	lines := splitLines(string(content))
	if line < 1 || line > len(lines) {
		return nil, "no patch: the location's line is not in the file"
	}
	// The failing statement can start a few lines above the line the error points at.
	for k := line - 1; k >= max(line-3, 0); k-- {
		s, e, found := findLiteral(strings.TrimRight(lines[k], "\r\n"), fix)
		if !found {
			continue
		}
		old := lines[k]
		lines[k] = old[:s] + jsString(fix.To) + old[e:]
		return &Patch{File: rel, Line: k + 1, Diff: unifiedDiff(rel, splitLines(string(content)), lines, k)}, ""
	}
	return nil, fmt.Sprintf("no patch: the locator is not on line %d of %s", line, path.Base(rel))
}

func splitLocation(loc string) (file string, line int, ok bool) {
	i := strings.LastIndex(loc, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(loc[i+1:])
	if err != nil {
		return "", 0, false
	}
	return loc[:i], n, true
}

// generated says a path is a dependency or a build output: a copy of a test file there is not the suite's test.
func generated(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == "node_modules" || seg == "dist" {
			return true
		}
	}
	return false
}

// tracked says git tracks rel (relative to the checkout root dir) and it is not a dependency or build output.
func tracked(ctx context.Context, dir, rel string) bool {
	if generated(rel) {
		return false
	}
	_, err := gitOut(ctx, dir, nil, "ls-files", "--error-unmatch", "--", rel)
	return err == nil
}

func isTestFile(rel string, testFiles []string) bool {
	if generated(rel) {
		return false
	}
	for _, f := range testFiles {
		f = strings.TrimPrefix(path.Clean(filepath.ToSlash(f)), "./")
		if f != "." && (rel == f || strings.HasSuffix(rel, "/"+f)) {
			return true
		}
	}
	return false
}

// splitLines keeps each line's terminator.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// findLiteral finds the string literal of the locator call on the line: the byte span of the old text.
func findLiteral(line string, fix Fix) (start, end int, ok bool) {
	needle := fix.Method + "("
	for from := 0; ; {
		i := strings.Index(line[from:], needle)
		if i < 0 {
			return 0, 0, false
		}
		i += from
		from = i + len(needle)
		p := &jsParser{s: line}
		p.i = i
		name, args, okc := p.call()
		if !okc || name != fix.Method || len(args) == 0 || !args[0].isStr {
			continue
		}
		if fix.Method == "getByRole" {
			if args[0].str != fix.Role || len(args) < 2 || args[1].obj == nil {
				continue
			}
			if n, has := args[1].obj["name"]; has && n.isStr && n.str == fix.From {
				return p.nameS, p.nameE, true
			}
			continue
		}
		if args[0].str == fix.From {
			return p.arg0S, p.arg0E, true
		}
	}
}

// jsString writes s as a double-quoted string literal that parses the same in JavaScript and TypeScript: quotes,
// backslashes, backticks, dollar signs and line terminators cannot end or reopen it.
func jsString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

// unifiedDiff is the diff of one changed line (index k) with three lines of context.
func unifiedDiff(rel string, before, after []string, k int) string {
	lo, hi := max(k-3, 0), min(k+4, len(before))
	var b strings.Builder
	fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", rel, rel, rel, rel, lo+1, hi-lo, lo+1, hi-lo)
	emit := func(prefix string, l string) {
		b.WriteString(prefix + l)
		if !strings.HasSuffix(l, "\n") {
			b.WriteString("\n\\ No newline at end of file\n")
		}
	}
	for i := lo; i < hi; i++ {
		if i == k {
			emit("-", before[i])
			emit("+", after[i])
			continue
		}
		emit(" ", before[i])
	}
	return b.String()
}

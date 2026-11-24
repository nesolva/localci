package workflow

import (
	"strings"
	"testing"
)

func TestFilterGlobSemantics(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		file     string
		want     bool
	}{
		{[]string{"**.go"}, "pkg/a.go", true},
		{[]string{"**.go"}, "a.go", true},
		{[]string{"**.go"}, "a.ts", false},
		{[]string{"src/**.ts"}, "src/a/b.ts", true},
		{[]string{"src/**.ts"}, "lib/a.ts", false},
		{[]string{"*.md"}, "README.md", true},
		{[]string{"*.md"}, "docs/README.md", false},
		{[]string{"docs/**"}, "docs/a/b.md", true},
		{[]string{"docs/**"}, "other/a.md", false},
		{[]string{"**/*.js"}, "a.js", true},
		{[]string{"**/*.js"}, "x/y/a.js", true},
		{[]string{"a?.txt"}, "a.txt", true},
		{[]string{"a+.txt"}, "aaa.txt", true},
		{[]string{"v[0-9].txt"}, "v3.txt", true},
		{[]string{"**", "!docs/**"}, "docs/a.md", false},
		{[]string{"**", "!docs/**"}, "src/a.md", true},
		{[]string{"**", "!docs/**", "docs/keep.md"}, "docs/keep.md", true}, // a later pattern re-includes
		{[]string{"docs/**", "!**.md"}, "docs/a.md", false},
	} {
		if got := pathSelected(tc.patterns, tc.file); got != tc.want {
			t.Errorf("%v on %q: got %v, want %v", tc.patterns, tc.file, got, tc.want)
		}
	}
}

func TestBranchFilters(t *testing.T) {
	tr := &trigger{branches: []string{"release/**", "main", "!release/old/**"}}
	for b, want := range map[string]bool{"release/1.0": true, "release/x/y": true, "release/old/1": false, "main": true, "feature/x": false} {
		if got := tr.skipReason(Event{Branch: b}) == ""; got != want {
			t.Errorf("branch %q fires=%v, want %v", b, got, want)
		}
	}
	if (&trigger{branches: []string{"release/*"}}).skipReason(Event{Branch: "release/a/b"}) == "" {
		t.Error("release/* must not match release/a/b")
	}
}

func TestPathsFilterOnWholeWorkflow(t *testing.T) {
	yml := "on:\n  pull_request:\n    paths: ['**.go']\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: make\n"
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b", Files: []string{"pkg/a.go"}},
		map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
	if len(res.Checks) != 1 {
		t.Errorf("**.go must select pkg/a.go, got %+v", res)
	}
}

func TestInvalidFilterPatternPlansWithWarning(t *testing.T) {
	for _, filter := range []string{
		"paths: ['[z-a]']",
		"paths-ignore: ['[z-a]']",
		"branches: ['[]']",
		"branches-ignore: ['[]']",
	} {
		yml := "on:\n  pull_request:\n    " + filter + "\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: make\n"
		res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b", Branch: "main", Files: []string{"a.go"}},
			map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
		if len(res.Checks) != 1 {
			t.Errorf("%s: an invalid pattern must plan the check, got %+v", filter, res)
			continue
		}
		pat := "[z-a]"
		if strings.Contains(filter, "[]") {
			pat = "[]"
		}
		if !strings.Contains(res.Checks[0].Unsupported, pat) {
			t.Errorf("%s: Unsupported must name the pattern, got %q", filter, res.Checks[0].Unsupported)
		}
	}
}

func TestFilterNonASCIIPatterns(t *testing.T) {
	if !pathSelected([]string{"docs/é.md"}, "docs/é.md") {
		t.Error("paths docs/é.md must select docs/é.md")
	}
	if !pathSelected([]string{"docs/日本?.md"}, "docs/日本.md") {
		t.Error("? after a multibyte rune must quantify the whole rune")
	}
	if (&trigger{branches: []string{"función/**"}}).skipReason(Event{Branch: "función/x"}) != "" {
		t.Error("branches función/** must select función/x")
	}
}

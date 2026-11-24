package triage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/store"
)

// A broken selector can be hundreds of characters long (a name built from page text); the fix line shares a
// three-line evidence with the others and keeps to their 60-character bound, while the patch itself keeps the
// whole value it replaces.
func TestFixLineClipsALongSelectorButThePatchKeepsItWhole(t *testing.T) {
	long := "Pay " + strings.Repeat("now ", 125) // about 500 characters
	spec := strings.Replace(checkoutSpec, "'Pay'", "'"+long+"'", 1)
	dir, head := repoWith(t, map[string]string{"e2e/checkout.spec.ts": spec})
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.PutRun(ctx, store.Run{ID: "1", Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed})
	st.PutTest(ctx, store.Test{Run: "1", ID: "t1", File: "checkout.spec.ts", Title: "pays", Status: "failed"})

	loc := filepath.Join(dir, "e2e/checkout.spec.ts") + ":6"
	in := RunInput{Run: "1", Tests: []string{"t1"}, Steps: map[string]int{"t1": 3}, LogDir: t.TempDir(),
		Workspace: Workspace{Dir: dir, Files: []string{"checkout.spec.ts"}, Shas: []string{head}},
		Attempts: map[string][]Attempt{"t1": {failed(report.Failure{
			Message:  "Test timeout of 30000ms exceeded.\nLocator: getByRole('button', { name: '" + long + "' })\nwaiting for getByRole('button', { name: '" + long + "' })",
			Aria:     sp(renamedAria),
			Location: sp(loc),
		})}}}
	if _, err := Run(ctx, st, in); err != nil {
		t.Fatal(err)
	}
	row, err := st.RunTest(ctx, "1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	ev := ReadEvidence(row.Evidence)
	var fix string
	for _, l := range ev.Lines {
		if strings.HasPrefix(l, "fix: ") {
			fix = l
		}
	}
	if fix == "" || ev.Patch == nil {
		t.Fatalf("evidence = %+v, want a fix line and its patch", ev)
	}
	// The line is "fix: replace <from> with <to> in <file> (...)": only the quoted values are bounded.
	from := fix[strings.Index(fix, `"`):strings.Index(fix, ` with `)]
	if n := utf8.RuneCountInString(from); n > 63 { // 60 characters, the ellipsis clip leaves, and the quotes
		t.Fatalf("the replaced value prints %d characters: %q", n, from)
	}
	if ev.Patch.From != long {
		t.Fatalf("the patch lost part of the selector: %q", ev.Patch.From)
	}
}

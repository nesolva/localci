package web

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

func TestFindingsPageListsTheLatestNightFirstWorstFirstAndEscapesWhatTheAgentWrote(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"old", "new"} {
		must(t, f.st.PutRun(ctx, store.Run{ID: id, Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed, Created: 1}))
	}
	must(t, f.st.AddFindings(ctx, []store.Finding{
		{Run: "old", Suite: "e2e", Severity: "low", Title: "Old typo", TS: 1000},
		{Run: "new", Suite: "e2e", Severity: "low", Title: "Footer link is dead", Goal: "buy as a new customer", TS: 5000},
		{Run: "new", Suite: "e2e", Severity: "high", Title: "<b>Card payment</b> returns 500", Steps: []string{"add an item", "pay"}, Test: "/x/drafts/finding-2.spec.ts", TS: 5000},
	}))
	_, page, _ := get(t, serve(t, f), "/findings")
	mustContain(t, page, "Findings", "&lt;b&gt;Card payment&lt;/b&gt; returns 500", "add an item", "finding-2.spec.ts", "Old typo")
	if strings.Contains(page, "<b>Card payment") {
		t.Error("a finding title is rendered as markup")
	}
	high, low, old := strings.Index(page, "Card payment"), strings.Index(page, "Footer link"), strings.Index(page, "Old typo")
	if !(high < low && low < old) {
		t.Errorf("order: high %d, low %d, older night %d; want worst first within the night, newest night first", high, low, old)
	}
	_, empty, _ := get(t, serve(t, newFixture(t)), "/findings")
	mustContain(t, empty, "localci explore")
}

func TestFindingScreenshotIsServedOnlyFromTheRunsOutputDirectory(t *testing.T) {
	f := newFixture(t)
	logs := t.TempDir()
	out := filepath.Join(logs, "explore.out")
	must(t, os.MkdirAll(out, 0o755))
	inside, outside := filepath.Join(out, "a.png"), filepath.Join(f.dir, "secret.png")
	must(t, os.WriteFile(inside, []byte("\x89PNG ok"), 0o644))
	must(t, os.WriteFile(outside, []byte("\x89PNG private"), 0o644))
	must(t, f.st.PutRun(ctx, store.Run{ID: "r", Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed, Created: 1}))
	must(t, f.st.PutShard(ctx, store.Shard{Run: "r", N: 1, State: "passed", LogPath: filepath.Join(logs, "explore.log")}))
	fs := []store.Finding{{Run: "r", Severity: "low", Title: "in", Screenshot: inside}, {Run: "r", Severity: "low", Title: "out", Screenshot: outside}}
	must(t, f.st.AddFindings(ctx, fs))
	srv := serve(t, f)
	if code, body, hdr := get(t, srv, "/findings/"+strconv.FormatInt(fs[0].ID, 10)+"/screenshot"); code != 200 || !strings.Contains(body, "ok") || hdr.Get("Content-Type") != "image/png" {
		t.Errorf("inside: status %d type %q", code, hdr.Get("Content-Type"))
	}
	if code, body, _ := get(t, srv, "/findings/"+strconv.FormatInt(fs[1].ID, 10)+"/screenshot"); code != 404 || strings.Contains(body, "private") {
		t.Errorf("outside the output directory: status %d, want 404 without the file", code)
	}
	if code, _, _ := get(t, srv, "/findings/999/screenshot"); code != 404 {
		t.Errorf("unknown finding: status %d", code)
	}
}

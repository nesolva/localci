package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

// The path is checked, then opened; a directory swapped for a link to somewhere else in between, the output dir or
// one inside it, must not be followed.
func TestArtifactParentDirSwappedForALinkAfterTheCheckIsNotServed(t *testing.T) {
	for _, c := range []struct{ name, rel string }{
		{"output dir", "ok.png"},
		{"directory inside it", filepath.Join("sub", "ok.png")},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			dir := filepath.Join(f.dir, "rd")
			out := filepath.Join(dir, "shard-1.out")
			must(t, os.MkdirAll(filepath.Join(out, "sub"), 0o755))
			must(t, f.st.PutRun(ctx, store.Run{ID: "rd", Suite: "web", Kind: store.KindSuite, State: store.StateFailed, Created: 1}))
			must(t, f.st.PutShard(ctx, store.Shard{Run: "rd", N: 1, State: "failed", LogPath: filepath.Join(dir, "shard-1.log")}))
			shot := filepath.Join(out, c.rel)
			must(t, os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nreal"), 0o644))
			// Somewhere else, a directory holding a file of the same name.
			elsewhere := filepath.Join(f.dir, "elsewhere")
			must(t, os.MkdirAll(elsewhere, 0o755))
			must(t, os.WriteFile(filepath.Join(elsewhere, "ok.png"), []byte("\x89PNG\r\n\x1a\nSECRET"), 0o644))
			must(t, f.st.PutTest(ctx, store.Test{Run: "rd", ID: "t", Title: "t", Status: "failed"}))
			must(t, f.st.PutArtifact(ctx, store.Artifact{Run: "rd", Test: "t", Kind: "screenshot", Path: shot}))
			srv := serve(t, f)

			if code, body, _ := get(t, srv, "/runs/rd/tests/t/screenshot"); code != 200 || !strings.Contains(body, "real") {
				t.Fatalf("before the swap: %d", code)
			}
			beforeArtifactOpen = func(string) {
				swap := out
				if strings.Contains(c.rel, "sub") {
					swap = filepath.Join(out, "sub")
				}
				must(t, os.Rename(swap, swap+".moved"))
				must(t, os.Symlink(elsewhere, swap))
			}
			t.Cleanup(func() { beforeArtifactOpen = func(string) {} })
			code, body, _ := get(t, srv, "/runs/rd/tests/t/screenshot")
			if code != 404 || strings.Contains(body, "SECRET") {
				t.Errorf("swapped directory: status %d, body served %q", code, body)
			}
		})
	}
}

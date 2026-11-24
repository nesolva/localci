package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

type fixture struct {
	st  *store.Store
	bus *event.Bus
	log *event.Log
	dir string
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := event.NewBus()
	return &fixture{st: st, bus: bus, log: event.NewLog(st, bus), dir: dir}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var ctx = context.Background()

// seedWide records a finished run of n tests over `shards` shards, four steps each, one second per test.
func seedWide(t testing.TB, f *fixture, id string, n, shards int) {
	must(t, f.st.PutRun(ctx, store.Run{ID: id, Suite: "web", Kind: store.KindSuite, State: store.StatePassed,
		Created: 1000, Started: 1000, Finished: 1000 + int64(n/shards)*1000 + 500}))
	for s := 1; s <= shards; s++ {
		must(t, f.st.PutShard(ctx, store.Shard{Run: id, N: s, State: store.StatePassed}))
	}
	for i := 0; i < n; i++ {
		shard := i%shards + 1
		slot := int64(i / shards)
		tid := fmt.Sprintf("t%04d", i)
		must(t, f.st.PutTest(ctx, store.Test{Run: id, Shard: shard, ID: tid, File: "e2e/a.spec.ts", Title: "case " + tid,
			Status: "passed", MS: 1000}))
		for k := 0; k < 4; k++ {
			sid := fmt.Sprintf("%s-%d", tid, k)
			must(t, f.st.PutStep(ctx, store.Step{Run: id, Test: tid, ID: sid, Title: fmt.Sprintf("step %d", k), Kind: "step",
				Start: 1000 + slot*1000 + int64(k)*250, MS: 250}))
			must(t, f.st.EndStep(ctx, id, tid, sid, 250, true))
		}
	}
}

// seedStory is a small, varied store: one run going, one queued, one failed with a full report, one passed.
func seedStory(t testing.TB, f *fixture) (failedRun, failedTest string) {
	now := time.Now().UnixMilli()
	// Running: two shards, one of them half done.
	must(t, f.st.PutRun(ctx, store.Run{ID: "run-live", Suite: "web", Kind: store.KindSuite, State: store.StateRunning,
		Trigger: "pr #42", RequestedBy: "ana", Created: now - 90_000, Started: now - 80_000}))
	for _, sh := range []store.Shard{{Run: "run-live", N: 1, State: "running"}, {Run: "run-live", N: 2, State: "running"}} {
		must(t, f.st.PutShard(ctx, sh))
	}
	for i, st := range []string{"passed", "passed", "running", "passed", "queued", "queued"} {
		must(t, f.st.PutTest(ctx, store.Test{Run: "run-live", Shard: i%2 + 1, ID: fmt.Sprintf("l%d", i), Title: "live " + st, Status: st, MS: 4000}))
	}
	must(t, f.st.PutStep(ctx, store.Step{Run: "run-live", Test: "l2", ID: "s1", Title: "open checkout", Kind: "step", Start: now - 5000}))

	must(t, f.st.PutRun(ctx, store.Run{ID: "run-wait", Suite: "web", Kind: store.KindSuite, State: store.StateQueued,
		Trigger: "push", RequestedBy: "bo", Created: now - 20_000}))
	must(t, f.st.PutRun(ctx, store.Run{ID: "run-ok", Suite: "api", Kind: store.KindSuite, State: store.StatePassed,
		Trigger: "manual", Created: now - 600_000, Started: now - 590_000, Finished: now - 500_000}))

	// Failed run with a failure report, a serial test and artifacts on disk.
	must(t, os.MkdirAll(filepath.Join(f.dir, "shard-1.out"), 0o755))
	shot := filepath.Join(f.dir, "shard-1.out", "fail.png")
	must(t, os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nnot really"), 0o644))
	trace := filepath.Join(f.dir, "trace dir", "t.zip")
	failure, _ := json.Marshal(map[string]any{
		"step": "Pay › click Place order", "message": "Timed out waiting for button",
		"expected": "enabled", "actual": "disabled", "location": "/work/e2e/pay.spec.ts:31",
		"aria":       "- main\n  - button \"Place order\" [disabled]",
		"console":    []string{"TypeError: x is undefined"},
		"network":    []map[string]any{{"method": "POST", "url": "http://localhost/api/orders", "status": 500}},
		"screenshot": shot, "trace": trace,
	})
	must(t, f.st.PutRun(ctx, store.Run{ID: "run-bad", Suite: "web", Kind: store.KindSuite, State: store.StateFailed,
		Trigger: "pr #41", RequestedBy: "cy", Created: now - 300_000, Started: now - 290_000, Finished: now - 200_000}))
	must(t, f.st.PutShard(ctx, store.Shard{Run: "run-bad", N: 1, State: "failed", LogPath: filepath.Join(f.dir, "shard-1.log")}))
	must(t, os.WriteFile(filepath.Join(f.dir, "shard-1.log"), []byte("starting\nrunning 2 tests\nError: boom in checkout\nok done\n"), 0o644))
	must(t, f.st.PutTest(ctx, store.Test{Run: "run-bad", Shard: 1, ID: "pay1", File: "e2e/pay.spec.ts", Line: 30, Title: "Pay › <b>places</b> an order",
		Status: "failed", MS: 3000, Failure: failure, Class: "product bug", Evidence: json.RawMessage(`{"reruns":"failed 2 of 2"}`)}))
	must(t, f.st.PutTest(ctx, store.Test{Run: "run-bad", Shard: 1, ID: "ok1", Title: "Home loads", Status: "passed", MS: 800}))
	must(t, f.st.PutTest(ctx, store.Test{Run: "run-bad", Shard: 1, ID: "ser1", Title: "Global banner", Status: "passed", MS: 500}))
	must(t, f.st.AddSerial(ctx, store.SerialEntry{Suite: "web", Test: "ser1"}))
	must(t, f.st.PutArtifact(ctx, store.Artifact{Run: "run-bad", Test: "pay1", Kind: "screenshot", Path: shot}))
	base := now - 290_000
	step := func(test, id, title string, off, ms int64, ok bool) {
		must(t, f.st.PutStep(ctx, store.Step{Run: "run-bad", Test: test, ID: id, Title: title, Kind: "step", Start: base + off, MS: ms}))
		must(t, f.st.EndStep(ctx, "run-bad", test, id, ms, ok))
	}
	step("ok1", "a", "open home", 0, 800, true)
	step("pay1", "b", "fill form", 900, 1200, true)
	step("pay1", "c", "click Place order", 2100, 1800, false)
	step("ser1", "d", "banner", 4000, 500, true)
	return "run-bad", "pay1"
}

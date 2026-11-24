package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// emit appends one report event the way the core stores it: the raw protocol line as data.
func emit(t testing.TB, f *fixture, run string, shard int, kind, test, step string, p map[string]any) {
	t.Helper()
	p["v"], p["t"], p["ts"] = 1, kind, time.Now().UnixMilli()
	raw, _ := json.Marshal(p)
	if _, err := f.st.AppendEvent(ctx, store.Event{Run: run, Shard: shard, Kind: kind, Test: test, Step: step, Data: raw}); err != nil {
		t.Fatal(err)
	}
}

// flowCall emits a flow's events the way the package does: a flow step, flow.begin, flow.end, step.end.
func flowCall(t testing.TB, f *fixture, run, test, step, parent, name, key, checkpoint string, ms int, inputs map[string]any, inner func()) {
	t.Helper()
	var par any
	if parent != "" {
		par = parent
	}
	emit(t, f, run, 1, "step.begin", test, step, map[string]any{"test": test, "step": step, "parent": par, "title": "flow: " + name, "kind": "flow"})
	emit(t, f, run, 1, "flow.begin", test, step, map[string]any{"test": test, "step": step, "flow": name, "inputs": inputs, "key": key})
	if inner != nil {
		inner()
	}
	emit(t, f, run, 1, "flow.end", test, step, map[string]any{"step": step, "outputs": map[string]any{}, "checkpoint": checkpoint, "ms": ms})
	emit(t, f, run, 1, "step.end", test, step, map[string]any{"step": step, "ms": ms, "ok": true})
}

func seedFlowRun(t testing.TB, f *fixture) {
	t.Helper()
	must(t, f.st.PutRun(ctx, store.Run{ID: "rf", Suite: "web", Kind: store.KindSuite, State: store.StatePassed, Created: time.Now().UnixMilli() - 6000, Started: time.Now().UnixMilli() - 5000, Finished: time.Now().UnixMilli() + 4000}))
	for _, id := range []string{"t1", "t2", "t3"} {
		must(t, f.st.PutTest(ctx, store.Test{Run: "rf", Shard: 1, ID: id, Title: "case " + id, Status: "passed", MS: 500}))
		emit(t, f, "rf", 1, "test.begin", id, "", map[string]any{"test": id, "title": "case " + id})
	}
	in := map[string]any{"user": "acme-admin", "password": "***"}
	// t1 builds the login checkpoint; the org was restored from an earlier run.
	flowCall(t, f, "rf", "t1", "t1:f1", "", "login", "aaa", "saved", 1200, in, nil)
	flowCall(t, f, "rf", "t1", "t1:f2", "", "org", "bbb", "restored", 40, nil, nil)
	flowCall(t, f, "rf", "t2", "t2:f1", "", "login", "aaa", "restored", 30, in, nil)
	flowCall(t, f, "rf", "t2", "t2:f2", "", "org", "bbb", "restored", 20, nil, nil)
	// t3 calls a flow that calls another inside it.
	flowCall(t, f, "rf", "t3", "t3:f1", "", "login", "aaa", "restored", 30, in, nil)
	flowCall(t, f, "rf", "t3", "t3:f2", "", "wrap", "ccc", "saved", 900, nil, func() {
		flowCall(t, f, "rf", "t3", "t3:f3", "t3:f2", "inner", "ddd", "saved", 300, nil, nil)
	})
	// A report line that claims to be a flow but is stored as invalid never becomes a node.
	emit(t, f, "rf", 1, "report.invalid", "t3", "t3:f9", map[string]any{"test": "t3", "step": "t3:f9", "flow": "evil", "key": "zzz"})
}

func TestDAGHasOneNodePerFlowKeyAndEdgesFromWhatRanBefore(t *testing.T) {
	f := newFixture(t)
	seedFlowRun(t, f)
	evs, _ := f.st.RunEvents(ctx, "rf")
	tests, _ := f.st.RunTests(ctx, "rf")
	byID := map[string]store.Test{}
	for _, x := range tests {
		byID[x.ID] = x
	}
	g := buildDAG(parseRunEvents(evs), -1, byID, false)

	names := map[string]*dagNode{}
	for _, n := range g.Nodes {
		names[n.Name] = n
	}
	if len(g.Nodes) != 4 || names["evil"] != nil {
		t.Fatalf("nodes %d (%v), want login, org, wrap, inner and never the invalid report", len(g.Nodes), names)
	}
	type e struct{ from, to string }
	got := map[e]bool{}
	for _, ed := range g.Edges {
		got[e{ed.From, ed.To}] = true
	}
	for _, want := range []e{{"aaa", "bbb"}, {"aaa", "ccc"}, {"ccc", "ddd"}} {
		if !got[want] {
			t.Errorf("missing edge %v; have %v", want, got)
		}
	}
	if len(g.Edges) != 3 {
		t.Errorf("edges %d, want 3", len(g.Edges))
	}
	if n := names["org"]; n.State != "restored" || n.Calls != 2 || n.Restored != 2 {
		t.Errorf("org: %+v, want restored in both calls", n)
	}
	if n := names["login"]; n.State != "ran" || n.Calls != 3 || n.Restored != 2 {
		t.Errorf("login: %+v, want 1 run and 2 restores", n)
	}
	if names["login"].Layer != 0 || names["wrap"].Layer != 1 || names["inner"].Layer != 2 {
		t.Errorf("layers login %d wrap %d inner %d", names["login"].Layer, names["wrap"].Layer, names["inner"].Layer)
	}
	// Tests hang on the last flow their body called, not on one nested inside it.
	if len(names["org"].Tests) != 2 || len(names["wrap"].Tests) != 1 || len(names["inner"].Tests) != 0 || g.Tests != 3 {
		t.Errorf("leaves: org %d wrap %d inner %d total %d", len(names["org"].Tests), len(names["wrap"].Tests), len(names["inner"].Tests), g.Tests)
	}
	if names["login"].Inputs != "password=***, user=acme-admin" {
		t.Errorf("inputs line %q", names["login"].Inputs)
	}
}

func TestDAGPageShowsNodesMarksRestoredOnesAndLinksTheTimeline(t *testing.T) {
	f := newFixture(t)
	seedFlowRun(t, f)
	code, page, _ := get(t, serve(t, f), "/runs/rf/dag")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, page, `data-key="aaa"`, `data-key="ccc"`, `class="node s-restored"`, "restored ×2",
		"4 flows · 4 of 7 calls restored", `data-from="aaa" data-to="bbb"`, `data-flows="`, `/static/dag.js`)
	if strings.Contains(page, `data-key="zzz"`) || strings.Contains(page, "evil") {
		t.Error("an invalid report line became a node")
	}
	// The flow's step in the timeline carries the key of the flow it called, for the highlight.
	if !strings.Contains(page, `data-flows="aaa"`) {
		t.Error("the timeline's step does not name its flow")
	}
}

// A button inside a link, or a link inside a button, is not valid and breaks keyboards and screen readers: the flow's
// button and its test links must sit side by side.
func TestDAGNodeHasNoInteractiveElementInsideAnother(t *testing.T) {
	f := newFixture(t)
	seedFlowRun(t, f)
	_, page, _ := get(t, serve(t, f), "/runs/rf/dag")
	if !strings.Contains(page, `<button type="button" class="n-pick"`) || !strings.Contains(page, `class="d `) {
		t.Fatal("the DAG has no flow button or no test links to check")
	}
	if strings.Contains(page, `role="button"`) || strings.Contains(page, `class="node`+" ") && strings.Contains(page, `tabindex`) {
		t.Error("a node is a div posing as a button")
	}
	for rest := page; ; {
		i := strings.Index(rest, "<button")
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := strings.Index(rest, "</button>")
		if end < 0 {
			t.Fatal("unclosed button")
		}
		if inner := rest[len("<button"):end]; strings.Contains(inner, "<a ") || strings.Contains(inner, "<button") {
			t.Errorf("interactive element inside a button: %.80s", inner)
		}
		rest = rest[end:]
	}
}

func TestDAGWithNoFlowsSaysSo(t *testing.T) {
	f := newFixture(t)
	seedStory(t, f)
	code, page, _ := get(t, serve(t, f), "/runs/run-bad/dag")
	if code != 200 || !strings.Contains(page, "No test in this run called a flow") {
		t.Errorf("status %d", code)
	}
}

// A live flow event reaches the open stream and the next render of the DAG page shows its node, within a second.
func TestDAGLiveFlowEventAddsItsNodeWithinASecond(t *testing.T) {
	f := newFixture(t)
	must(t, f.st.PutRun(ctx, store.Run{ID: "rl", Suite: "web", Kind: store.KindSuite, State: store.StateRunning, Created: time.Now().UnixMilli() - 6000, Started: time.Now().UnixMilli() - 5000}))
	emit(t, f, "rl", 1, "test.begin", "t1", "", map[string]any{"test": "t1"})
	srv := serve(t, f)
	_, page, _ := get(t, srv, "/runs/rl/dag")
	if strings.Contains(page, `data-key="k1"`) {
		t.Fatal("node before its event")
	}
	seq := between(page, `data-seq="`, `"`)
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, "GET", srv.URL+"/live?since="+seq, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := make(chan struct{}, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"kind":"flow.begin"`) {
				got <- struct{}{}
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	emit(t, f, "rl", 1, "step.begin", "t1", "t1:f1", map[string]any{"test": "t1", "step": "t1:f1", "title": "flow: login", "kind": "flow"})
	raw, _ := json.Marshal(map[string]any{"v": 1, "t": "flow.begin", "test": "t1", "step": "t1:f1", "flow": "login", "inputs": map[string]any{}, "key": "k1"})
	if _, err := f.log.Append(cctx, store.Event{Run: "rl", Shard: 1, Kind: "flow.begin", Test: "t1", Step: "t1:f1", Data: raw}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the stream did not carry the flow event within 1 s")
	}
	_, page, _ = get(t, srv, "/runs/rl/dag")
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
	mustContain(t, page, `data-key="k1"`, "s-live", "login")
}

// A retry reuses the step ids of the first try, so only the events can tell them apart.
func seedRetry(t testing.TB, f *fixture, state string) {
	t.Helper()
	must(t, f.st.PutRun(ctx, store.Run{ID: "rr", Suite: "web", Kind: store.KindSuite, State: state, Created: time.Now().UnixMilli() - 6000, Started: time.Now().UnixMilli() - 5000}))
	must(t, f.st.PutTest(ctx, store.Test{Run: "rr", Shard: 1, ID: "tt", Title: "pays", Status: "failed", MS: 500, Retry: 1}))
	for try, title := range []string{"first-try step", "second-try step"} {
		emit(t, f, "rr", 1, "test.begin", "tt", "", map[string]any{"test": "tt", "retry": try})
		emit(t, f, "rr", 1, "step.begin", "tt", "tt:1", map[string]any{"test": "tt", "step": "tt:1", "title": title, "kind": "step"})
		if try == 0 {
			emit(t, f, "rr", 1, "step.end", "tt", "tt:1", map[string]any{"step": "tt:1", "ms": 100, "ok": false})
		}
	}
}

func TestATestWithRetriesShowsOnlyItsLastAttemptByDefault(t *testing.T) {
	f := newFixture(t)
	seedRetry(t, f, store.StateFailed)
	srv := serve(t, f)
	for _, path := range []string{"/runs/rr", "/runs/rr/dag", "/runs/rr/tests/tt"} {
		_, page, _ := get(t, srv, path)
		if !strings.Contains(page, "second-try step") || strings.Contains(page, "first-try step") {
			t.Errorf("%s: want the last attempt only", path)
		}
		if !strings.Contains(page, "attempt=1") {
			t.Errorf("%s: earlier attempts are not selectable", path)
		}
		_, page, _ = get(t, srv, path+"?attempt=1")
		if !strings.Contains(page, "first-try step") || strings.Contains(page, "second-try step") {
			t.Errorf("%s?attempt=1: want the first attempt only", path)
		}
	}
}

func TestAStepStillOpenWhenTheRunFinishedIsUnfinishedNotLive(t *testing.T) {
	f := newFixture(t)
	seedRetry(t, f, store.StateFailed) // the last attempt's step never ended
	srv := serve(t, f)
	_, page, _ := get(t, srv, "/runs/rr")
	mustContain(t, page, "is-open", "second-try step — did not finish")
	if strings.Contains(page, "is-live") {
		t.Error("a finished run draws a live step")
	}
	_, page, _ = get(t, srv, "/runs/rr/tests/tt")
	mustContain(t, page, "unfinished")

	// The same open step in a run still going is live; the superseded first attempt's is not.
	g := newFixture(t)
	seedRetry(t, g, store.StateRunning)
	_, page, _ = get(t, serve(t, g), "/runs/rr")
	mustContain(t, page, "is-live")
	_, page, _ = get(t, serve(t, g), "/runs/rr?attempt=1")
	if strings.Contains(page, "is-live") {
		t.Error("an earlier attempt's open step is drawn live")
	}
}

func TestScreenshotRouteServesOnlyArtifactsInsideTheRunsOutputDir(t *testing.T) {
	f := newFixture(t)
	mk := func(run string) string { // a run's log dir with one shard and its output folder
		dir := filepath.Join(f.dir, run)
		must(t, os.MkdirAll(filepath.Join(dir, "shard-1.out"), 0o755))
		must(t, f.st.PutRun(ctx, store.Run{ID: run, Suite: "web", Kind: store.KindSuite, State: store.StateFailed, Created: 1}))
		must(t, f.st.PutShard(ctx, store.Shard{Run: run, N: 1, State: "failed", LogPath: filepath.Join(dir, "shard-1.log")}))
		return dir
	}
	a, b := mk("ra"), mk("rb")
	png := []byte("\x89PNG\r\n\x1a\nx")
	inside := filepath.Join(a, "shard-1.out", "ok.png")
	other := filepath.Join(b, "shard-1.out", "theirs.png") // another run's
	outside := filepath.Join(a, "elsewhere.png")           // in the log dir but not an output dir
	linked := filepath.Join(a, "shard-1.out", "link.png")  // inside, but a link to a file outside
	traversal := filepath.Join(a, "shard-1.out", "..", "elsewhere.png")
	for _, p := range []string{inside, other, outside} {
		must(t, os.WriteFile(p, png, 0o644))
	}
	must(t, os.Symlink(outside, linked))
	for i, p := range []string{inside, other, outside, linked, traversal, "/etc/hosts"} {
		id := fmt.Sprintf("t%d", i)
		must(t, f.st.PutTest(ctx, store.Test{Run: "ra", ID: id, Title: id, Status: "failed"}))
		must(t, f.st.PutArtifact(ctx, store.Artifact{Run: "ra", Test: id, Kind: "screenshot", Path: p}))
	}
	srv := serve(t, f)
	for i, want := range []int{200, 404, 404, 404, 404, 404} {
		if code, _, _ := get(t, srv, fmt.Sprintf("/runs/ra/tests/t%d/screenshot", i)); code != want {
			t.Errorf("artifact %d: status %d, want %d", i, code, want)
		}
	}
}

func TestScreenshotCacheIsLongForFinishedRunsAndNoneForLiveOnes(t *testing.T) {
	f := newFixture(t)
	for _, run := range []struct{ id, state string }{{"done", store.StatePassed}, {"going", store.StateRunning}} {
		dir := filepath.Join(f.dir, run.id)
		must(t, os.MkdirAll(filepath.Join(dir, "shard-1.out"), 0o755))
		p := filepath.Join(dir, "shard-1.out", "s.png")
		must(t, os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nx"), 0o644))
		must(t, f.st.PutRun(ctx, store.Run{ID: run.id, Suite: "web", Kind: store.KindSuite, State: run.state, Created: 1}))
		must(t, f.st.PutShard(ctx, store.Shard{Run: run.id, N: 1, State: run.state, LogPath: filepath.Join(dir, "shard-1.log")}))
		must(t, f.st.PutTest(ctx, store.Test{Run: run.id, ID: "t", Title: "t", Status: "failed"}))
		must(t, f.st.PutArtifact(ctx, store.Artifact{Run: run.id, Test: "t", Kind: "screenshot", Path: p}))
	}
	srv := serve(t, f)
	_, _, h := get(t, srv, "/runs/done/tests/t/screenshot")
	if cc := h.Get("Cache-Control"); !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("finished run: %q", cc)
	}
	_, _, h = get(t, srv, "/runs/going/tests/t/screenshot")
	if cc := h.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("live run: %q", cc)
	}
}

func TestShellQuoteKeepsAnyPathAsOneWord(t *testing.T) {
	for in, want := range map[string]string{
		"/out/trace.zip":     "/out/trace.zip",
		"/out/a b/t.zip":     "'/out/a b/t.zip'",
		"/out/a\nb/t.zip":    "'/out/a\nb/t.zip'",
		"/out/it's/t.zip":    `'/out/it'\''s/t.zip'`,
		"/out/$(id)/t.zip":   "'/out/$(id)/t.zip'",
		"/out/été/trace.zip": "'/out/été/trace.zip'",
		"-rf x":              "'./-rf x'", // quoted it would still be read as options
		"--output=/tmp/x":    "./--output=/tmp/x",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// The path is checked, then opened; a link swapped in between must not be followed.
func TestScreenshotSwappedForALinkAfterTheCheckIsNotServed(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.dir, "rs")
	must(t, os.MkdirAll(filepath.Join(dir, "shard-1.out"), 0o755))
	must(t, f.st.PutRun(ctx, store.Run{ID: "rs", Suite: "web", Kind: store.KindSuite, State: store.StateFailed, Created: 1}))
	must(t, f.st.PutShard(ctx, store.Shard{Run: "rs", N: 1, State: "failed", LogPath: filepath.Join(dir, "shard-1.log")}))
	shot := filepath.Join(dir, "shard-1.out", "ok.png")
	secret := filepath.Join(f.dir, "secret.png")
	must(t, os.WriteFile(shot, []byte("\x89PNG\r\n\x1a\nreal"), 0o644))
	must(t, os.WriteFile(secret, []byte("\x89PNG\r\n\x1a\nSECRET"), 0o644))
	must(t, f.st.PutTest(ctx, store.Test{Run: "rs", ID: "t", Title: "t", Status: "failed"}))
	must(t, f.st.PutArtifact(ctx, store.Artifact{Run: "rs", Test: "t", Kind: "screenshot", Path: shot}))
	srv := serve(t, f)

	if code, body, _ := get(t, srv, "/runs/rs/tests/t/screenshot"); code != 200 || !strings.Contains(body, "real") {
		t.Fatalf("before the swap: %d", code)
	}
	beforeArtifactOpen = func(p string) {
		must(t, os.Remove(p))
		must(t, os.Symlink(secret, p))
	}
	t.Cleanup(func() { beforeArtifactOpen = func(string) {} })
	code, body, _ := get(t, srv, "/runs/rs/tests/t/screenshot")
	if code != 404 || strings.Contains(body, "SECRET") {
		t.Errorf("swapped link: status %d, body served %q", code, body)
	}
}

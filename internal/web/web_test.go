package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/store"
)

func serve(t *testing.T, f *fixture, opts ...Option) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(f.st, f.bus, opts...))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), res.Header
}

func mustContain(t *testing.T, page string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(page, w) {
			t.Errorf("page is missing %q", w)
		}
	}
}

func TestOverviewShowsWhatRunsWhoSentItTheQueueAndRecentRuns(t *testing.T) {
	f := newFixture(t)
	seedStory(t, f)
	code, page, _ := get(t, serve(t, f), "/")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, page,
		"pr #42 · ana", // the running run and who sent it
		"left",         // time left, from the run's own finished tests
		"1/3", "2/3",   // progress per shard
		"Queue", "push · bo", // the waiting run
		"1 failed", "manual", // recent runs: the failure count, a passed one without fuss
		`href="/runs/run-bad"`)
	if strings.Contains(page, "Nothing is running") {
		t.Error("says nothing is running while a run is")
	}
}

func TestOverviewWithNothingRunningSaysSo(t *testing.T) {
	f := newFixture(t)
	_, page, _ := get(t, serve(t, f), "/")
	mustContain(t, page, "Nothing is running")
}

func TestRunPageDrawsEachShardTestAndStepAndLinksTheFailure(t *testing.T) {
	f := newFixture(t)
	run, test := seedStory(t, f)
	code, page, _ := get(t, serve(t, f), "/runs/"+run)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, page,
		"Shard 1", ">Serial<", // the serial group has its own lane
		`data-tip="Home loads — 800 ms"`,
		`data-tip="click Place order — 1.8 s"`, // durations on hover
		`class="test fail"`, "is-fail",         // the failure is marked on the test and on its step
		`href="/runs/`+run+`/tests/`+test+`"`,
		`href="/runs/`+run+`/dag"`, // the DAG tab
		"1 failed", "product bug", "/runs/"+run+"/log?shard=1")
	// The serial test sits on the serial lane only, not also on its shard's lane.
	if n := strings.Count(page, `/tests/ser1"`); n != 1 {
		t.Errorf("serial test drawn %d times, want once", n)
	}
}

func TestTestPageHasTheFullFailureReport(t *testing.T) {
	f := newFixture(t)
	run, test := seedStory(t, f)
	srv := serve(t, f)
	code, page, _ := get(t, srv, "/runs/"+run+"/tests/"+test)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	mustContain(t, page,
		"Pay › click Place order", "enabled", "disabled", // step and expected vs actual
		`button &#34;Place order&#34; [disabled]`, "TypeError: x is undefined", // aria excerpt, console
		"POST", "/api/orders", "500", // network
		"/work/e2e/pay.spec.ts:31",
		`npx playwright show-trace &#39;`, // trace command, quoted because the path has a space
		"product bug", "failed 2 of 2",    // class and evidence when T-13 fills them
		`src="/runs/`+run+`/tests/`+test+`/screenshot"`)
	if strings.Contains(page, "<b>places</b>") {
		t.Error("a test title is rendered as markup")
	}
	mustContain(t, page, "&lt;b&gt;places&lt;/b&gt;")

	code, body, hdr := get(t, srv, "/runs/"+run+"/tests/"+test+"/screenshot")
	if code != 200 || !strings.HasPrefix(body, "\x89PNG") || hdr.Get("Content-Type") != "image/png" {
		t.Errorf("screenshot: status %d, type %q", code, hdr.Get("Content-Type"))
	}
}

// Only files the store holds are ever served: a failure that names some other file shows no screenshot.
func TestScreenshotRouteNeverServesAPathTheStoreDoesNotHold(t *testing.T) {
	f := newFixture(t)
	secret := filepath.Join(f.dir, "secret.png")
	must(t, os.WriteFile(secret, []byte("private"), 0o644))
	must(t, f.st.PutRun(ctx, store.Run{ID: "r", Suite: "web", Kind: store.KindSuite, State: store.StateFailed, Created: 1}))
	must(t, f.st.PutTest(ctx, store.Test{Run: "r", ID: "t", Title: "x", Status: "failed",
		Failure: []byte(`{"step":"s","message":"m","screenshot":"` + secret + `"}`)}))
	srv := serve(t, f)
	if code, body, _ := get(t, srv, "/runs/r/tests/t/screenshot"); code != 404 || strings.Contains(body, "private") {
		t.Errorf("status %d, want 404 without the file", code)
	}
	if _, page, _ := get(t, srv, "/runs/r/tests/t"); strings.Contains(page, "<img") {
		t.Error("the page shows a screenshot the store does not hold")
	}
}

func TestLogShowsFailingLinesFirstThenTheTail(t *testing.T) {
	f := newFixture(t)
	run, _ := seedStory(t, f)
	code, page, _ := get(t, serve(t, f), "/runs/"+run+"/log")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	hot, tail := strings.Index(page, "Failing lines"), strings.Index(page, "Full log")
	if hot < 0 || tail < hot || !strings.Contains(page, "Error: boom in checkout") {
		t.Errorf("failing lines at %d, log at %d", hot, tail)
	}
	if _, page, _ := get(t, serve(t, f), "/runs/run-ok/log"); !strings.Contains(page, "No log has been written") {
		t.Error("a run without a log says nothing about it")
	}
}

func TestSettingsShowsEffectiveConfigWithSourcesAndCapacity(t *testing.T) {
	f := newFixture(t)
	seedStory(t, f)
	cfg := filepath.Join(f.dir, "config.yaml")
	must(t, os.WriteFile(cfg, []byte("version: 1\nserver:\n  port: 8123\n"), 0o644))
	m := capacity.Machine{CPUs: 8, FreeMemory: 16 << 30}
	load := func() (*config.Effective, error) {
		return config.Load(config.Options{Path: cfg, Env: func(string) string { return "" }, Machine: m})
	}
	_, page, _ := get(t, serve(t, f, WithConfig(load), WithMachine(m)), "/settings")
	mustContain(t, page, "server.port", "8123", `src-file`, "check slots in use", "2<small>/")
}

func TestUnknownRunAndPageAre404(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	for _, p := range []string{"/runs/nope", "/runs/nope/tests/x", "/elsewhere"} {
		if code, _, _ := get(t, srv, p); code != 404 {
			t.Errorf("%s: status %d, want 404", p, code)
		}
	}
}

// What a page follows: an event appended after the page was rendered reaches its stream at once.
func TestAnAppendedEventReachesTheOpenStreamWithinASecond(t *testing.T) {
	f := newFixture(t)
	seedStory(t, f)
	srv := serve(t, f)
	_, page, _ := get(t, srv, "/")
	seq := between(page, `data-seq="`, `"`)
	if seq == "" {
		t.Fatal("the page does not say where its stream starts")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live?since="+seq, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				got <- sc.Text()
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the stream attach; the replay from the cursor would cover a miss anyway
	start := time.Now()
	if _, err := f.log.Append(ctx, store.Event{TS: 1, Run: "run-live", Kind: "step.begin", Test: "l2"}); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-got:
		if d := time.Since(start); d > time.Second {
			t.Errorf("took %v", d)
		}
		if !strings.Contains(line, `"run":"run-live"`) {
			t.Errorf("unexpected event %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("no event within 1 s")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return ""
}

func TestPagesCarryTheSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	_, _, h := get(t, serve(t, f), "/")
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", h)
	}
}

// A report's times are data from the test process: a step that claims to last 100,000 hours, or to have begun in
// 1970, must not stretch the run's axis. The page keeps a handful of ticks and the honest steps stay readable.
func TestRunPageStaysSmallWhenAStepReportsAnAbsurdTime(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UnixMilli()
	must(t, f.st.PutRun(ctx, store.Run{ID: "rx", Suite: "web", Kind: store.KindSuite, State: store.StatePassed,
		Created: now - 10_000, Started: now - 9_000, Finished: now}))
	ev := func(kind, test, step string, ts int64, p map[string]any) {
		t.Helper()
		p["v"], p["t"], p["ts"] = 1, kind, ts
		raw, _ := json.Marshal(p)
		if _, err := f.st.AppendEvent(ctx, store.Event{TS: now, Run: "rx", Shard: 1, Kind: kind, Test: test, Step: step, Data: raw}); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		test   string
		ts, ms int64
	}{
		{"t1", now - 9_000, 1_000},           // honest
		{"t2", now - 8_000, 360_000_000_000}, // 100,000 hours
		{"t3", 1, 500},                       // began at the epoch
	} {
		must(t, f.st.PutTest(ctx, store.Test{Run: "rx", Shard: 1, ID: c.test, Title: "case " + c.test, Status: "passed"}))
		step := c.test + ":" + strconv.Itoa(i)
		ev("test.begin", c.test, "", c.ts, map[string]any{"test": c.test})
		ev("step.begin", c.test, step, c.ts, map[string]any{"test": c.test, "step": step, "title": "step of " + c.test, "kind": "step"})
		ev("step.end", c.test, step, c.ts, map[string]any{"step": step, "ms": c.ms, "ok": true})
	}

	code, page, _ := get(t, serve(t, f), "/runs/rx")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if n := strings.Count(page, `class="grid"`); n > 13 {
		t.Fatalf("the lane draws %d ticks (page is %d bytes); want at most 13", n, len(page))
	}
	m := regexp.MustCompile(`style="left:[0-9.]+%;width:([0-9.]+)%" data-tip="case t1 `).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("the honest test has no bar")
	}
	if w, _ := strconv.ParseFloat(m[1], 64); w < 5 {
		t.Errorf("the honest one-second test is %.4f%% wide in a ten-second run; absurd steps squashed it", w)
	}

	// An older run whose steps were stored without events draws from the steps table: same cap.
	must(t, f.st.PutRun(ctx, store.Run{ID: "ry", Suite: "web", Kind: store.KindSuite, State: store.StatePassed,
		Created: now - 10_000, Started: now - 9_000, Finished: now}))
	must(t, f.st.PutTest(ctx, store.Test{Run: "ry", Shard: 1, ID: "t2", Title: "case t2", Status: "passed"}))
	must(t, f.st.PutStep(ctx, store.Step{Run: "ry", Test: "t2", ID: "s", Title: "long", Kind: "step", Start: now - 8_000}))
	must(t, f.st.EndStep(ctx, "ry", "t2", "s", 360_000_000_000, true))
	_, page, _ = get(t, serve(t, f), "/runs/ry")
	if n := strings.Count(page, `class="grid"`); n == 0 || n > 13 {
		t.Fatalf("the stored lane draws %d ticks (page is %d bytes); want 1 to 13", n, len(page))
	}
}

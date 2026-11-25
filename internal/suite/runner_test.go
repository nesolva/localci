package suite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nesolva/localci/internal/store"
	_ "modernc.org/sqlite"
)

// stored is one attempt of a test as the store holds it.
type stored struct {
	Test, Status string
	Shard, Retry int
	Evidence     string
}

// attempts reads every stored attempt of the run (the store API lists failures only).
func (g *rig) attempts(run string) []stored { return readAttempts(g.t, g.data, run) }

func readAttempts(t *testing.T, dataDir, run string) []stored {
	t.Helper()
	g := struct{ t *testing.T }{t}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "store.db")+"?mode=ro")
	if err != nil {
		g.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT test, status, shard, retry, COALESCE(evidence,'') FROM tests WHERE run=? ORDER BY rowid`, run)
	if err != nil {
		g.t.Fatal(err)
	}
	defer rows.Close()
	var out []stored
	for rows.Next() {
		var s stored
		if err := rows.Scan(&s.Test, &s.Status, &s.Shard, &s.Retry, &s.Evidence); err != nil {
			g.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// last is the final attempt of each test.
func last(all []stored) map[string]stored {
	m := map[string]stored{}
	for _, a := range all {
		if a.Retry >= m[a.Test].Retry {
			m[a.Test] = a
		}
	}
	return m
}

func (g *rig) mustPass(run store.Run) {
	g.t.Helper()
	if run.State != store.StatePassed {
		g.t.Fatalf("run %s is %s (%s), want passed", run.ID, run.State, run.Reason)
	}
}

func TestRunStoresEveryTestOnItsOwnShard(t *testing.T) {
	ids := []string{"pass1", "pass2", "pass3", "pass4", "pass5", "pass6"}
	g := newRig(t, fakeSuite(""), ids, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)

	got := last(g.attempts(run.ID))
	if len(got) != len(ids) {
		t.Fatalf("stored %d tests, want %d: %v", len(got), len(ids), got)
	}
	for _, id := range ids {
		if got[id].Status != "passed" {
			t.Errorf("%s: %s", id, got[id].Status)
		}
	}

	// Every shard ran with its own number, database and port.
	lines := readLines(filepath.Join(g.shared, "shards.log"))
	if len(lines) != 2 {
		t.Fatalf("want one invocation per shard, got %q", lines)
	}
	seen := map[string]map[string]bool{"shard": {}, "db": {}, "port": {}}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 4 || f[1] == "" || f[2] == "" || f[2] == "0" {
			t.Fatalf("incomplete shard environment: %q", l)
		}
		seen["shard"][f[0]], seen["db"][f[1]], seen["port"][f[2]] = true, true, true
	}
	for k, v := range seen {
		if len(v) != 2 {
			t.Errorf("shards share a %s: %q", k, lines)
		}
	}

	// The timeline has a lane per shard with the steps in it, and nothing is left behind.
	lanes, err := g.st.Timeline(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 2 {
		t.Fatalf("timeline lanes = %d, want 2", len(lanes))
	}
	steps := 0
	for _, l := range lanes {
		steps += len(l.Steps)
	}
	if steps != len(ids) {
		t.Errorf("timeline has %d steps, want %d", steps, len(ids))
	}
	if left := g.dbFiles(); len(left) != 0 {
		t.Errorf("shard databases left behind: %v", left)
	}
	for _, p := range pidsIn(filepath.Join(g.shared, "stack.pids")) {
		waitFor(t, "stack process to end", func() bool { return !alive(p) })
	}
}

func TestFailedTestKeepsItsFailureReport(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1", "broken1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "1 of 2 tests failed") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	fails, err := g.st.Failures(context.Background(), run.ID)
	if err != nil || len(fails) != 1 || fails[0].ID != "broken1" {
		t.Fatalf("failures = %+v, %v", fails, err)
	}
	var f struct {
		Message string
		Aria    string
		Console []string
		Network []struct{ Status int }
	}
	if err := json.Unmarshal(fails[0].Failure, &f); err != nil {
		t.Fatal(err)
	}
	if f.Message != "boom" || f.Aria == "" || len(f.Console) == 0 || len(f.Network) == 0 || f.Network[0].Status != 500 {
		t.Errorf("failure report lost evidence: %s", fails[0].Failure)
	}
	// A test that fails alone too is a real failure, not a conflict.
	if s, _ := g.st.Serial(context.Background(), "e2e"); len(s) != 0 {
		t.Errorf("a plain failure joined the serial group: %+v", s)
	}
}

func TestConflictMovesToSerialGroupAndPassesNextTime(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"setter1", "reader1"}, 4)
	ctx := context.Background()

	first := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(first) // the reader fails next to the setter, passes alone: it counts as passed
	if !strings.Contains(first.Reason, MovedNote) {
		t.Errorf("reason %q lacks %q", first.Reason, MovedNote)
	}
	all := g.attempts(first.ID)
	if r := last(all)["reader1"]; r.Status != "passed" || !strings.Contains(r.Evidence, MovedNote) || r.Retry < 1 {
		t.Errorf("reader1 final attempt = %+v (all %+v)", r, all)
	}
	serial, err := g.st.Serial(ctx, "e2e")
	if err != nil || len(serial) != 1 || serial[0].Test != "reader1" || !strings.Contains(string(serial[0].Evidence), "setter1") {
		t.Fatalf("serial group = %+v, %v", serial, err)
	}

	second := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(second)
	if strings.Contains(second.Reason, MovedNote) {
		t.Errorf("the second run moved it again: %q", second.Reason)
	}
	got := last(g.attempts(second.ID))
	if got["reader1"].Shard != 0 || got["reader1"].Status != "passed" || got["setter1"].Shard == 0 {
		t.Errorf("second run: reader1 %+v, setter1 %+v; want the reader alone on shard 0", got["reader1"], got["setter1"])
	}
}

func TestArtifactsOutliveRerunsAndTheNextRun(t *testing.T) {
	t.Setenv("FAKE_ARTIFACT", "1")
	g := newRig(t, fakeSuite(""), []string{"setter1", "reader1", "pass1"}, 4)
	ctx := context.Background()
	exist := func(run string) {
		t.Helper()
		arts, err := g.st.Artifacts(ctx, run)
		if err != nil || len(arts) == 0 {
			t.Fatalf("run %s artifacts = %v, %v", run, arts, err)
		}
		for _, a := range arts {
			if _, err := os.Stat(a.Path); err != nil {
				t.Errorf("run %s: recorded artifact is gone: %s", run, a.Path)
			}
		}
	}
	first := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(first) // reader1 fails beside setter1, then reruns alone on shard 0
	exist(first.ID)
	second := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(second)
	exist(second.ID)
	exist(first.ID) // the second run's checkout cleaned the workspace
}

func TestFailedCloneDropsNothing(t *testing.T) {
	// A clone that fails (say, the name exists: it is another run's database) must not drop that database.
	yaml := strings.Replace(fakeSuite(""), `create: "cp {{template}} {{name}}"`,
		`create: 'case {{name}} in *tpl*) cp {{template}} {{name}};; *) exit 1;; esac'`, 1)
	yaml = strings.Replace(yaml, `drop: "rm -f {{name}}"`, `drop: 'echo {{name}} >> "$FAKE_DIR/drops"; rm -f {{name}}'`, 1)
	g := newRig(t, yaml, []string{"pass1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "clone database") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	// Rebuilding the run's own unfinished template drops it first; any other drop hits a database this run never cloned.
	for _, name := range readLines(filepath.Join(g.shared, "drops")) {
		if name != "" && !strings.Contains(filepath.Base(name), "localci_tpl_") {
			t.Errorf("dropped a database this run never cloned: %q", name)
		}
	}
}

// The template seed may read the run's checkouts: {{dir "repo"}} is where the repository is checked out.
func TestTemplateSeedReadsTheCheckout(t *testing.T) {
	yaml := strings.Replace(fakeSuite(""), `seed: "echo seeded > {{db.url}}"`,
		`seed: 'cp {{dir "acme/web"}}/seed.sql {{db.url}}'`, 1)
	g := newRig(t, yaml, []string{"pass1"}, 4)
	g.commit(map[string]string{"seed.sql": "insert into acme\n"})
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))

	tpl, _ := filepath.Glob(filepath.Join(g.data, "state", "e2e", "db", "localci_tpl_*"))
	if len(tpl) != 1 {
		t.Fatalf("templates = %v, want one", tpl)
	}
	if b, _ := os.ReadFile(tpl[0]); string(b) != "insert into acme\n" {
		t.Fatalf("template holds %q, want the checkout's seed.sql", b)
	}
}

func TestPrepareRunsOnlyWhenItsKeyChanges(t *testing.T) {
	prep := `    prepare:
      - repo: acme/web
        run: 'echo x >> "$FAKE_DIR/prep.log"; mkdir -p out; echo built > out/x'
        cache_key: [lock.txt]
        keep: [out]
`
	g := newRig(t, fakeSuite(prep), []string{"pass1"}, 4)
	count := func() int { return len(readLines(filepath.Join(g.shared, "prep.log"))) }
	runOnce := func() { g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()})) }

	runOnce()
	if count() != 1 {
		t.Fatalf("first run: prepare ran %d times", count())
	}
	runOnce()
	if count() != 1 {
		t.Errorf("second run with the same key: prepare ran again (%d)", count())
	}
	g.commit(map[string]string{"lock.txt": "2\n"})
	runOnce()
	if count() != 2 {
		t.Errorf("key file changed: prepare ran %d times in all, want 2", count())
	}
}

func TestCancelLeavesNothingRunning(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"hang1", "hang2"}, 4)
	id, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both shards to hang", func() bool { return len(pidsIn(filepath.Join(g.shared, "pids"))) == 2 })
	if len(g.dbFiles()) != 2 {
		t.Fatalf("running shards should hold 2 databases, have %v", g.dbFiles())
	}
	if !g.r.Cancel(id) {
		t.Fatal("Cancel said the run was not active")
	}
	run := g.wait(id)
	if run.State != store.StateCancelled || run.Reason != "cancelled" {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	for _, p := range append(pidsIn(filepath.Join(g.shared, "pids")), pidsIn(filepath.Join(g.shared, "stack.pids"))...) {
		waitFor(t, fmt.Sprintf("process %d to end", p), func() bool { return !alive(p) })
	}
	if left := g.dbFiles(); len(left) != 0 {
		t.Errorf("shard databases left behind: %v", left)
	}
	if g.r.Cancel(id) {
		t.Error("a finished run still counts as active")
	}
}

func TestNewerRequestForTheSamePRSupersedes(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"hang1", "pass1"}, 4)
	git(t, g.src, "update-ref", "refs/pull/5/head", "HEAD")

	old, err := g.r.Submit(Request{Suite: "e2e", Heads: prHead(5)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first run to hang", func() bool { return len(pidsIn(filepath.Join(g.shared, "pids"))) > 0 })
	newer := g.run(Request{Suite: "e2e", Heads: prHead(5), Tests: Filter{IDs: []string{"pass1"}}})
	g.mustPass(newer)
	oldRun := g.wait(old)
	if oldRun.State != store.StateCancelled || oldRun.Reason != "superseded" {
		t.Errorf("old run = %s (%s), want cancelled (superseded)", oldRun.State, oldRun.Reason)
	}
}

func TestTimeoutFailsTheRunAndStopsEverything(t *testing.T) {
	g := newRig(t, fakeSuite("    timeout: 4s\n"), []string{"hang1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "timed out") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	for _, p := range pidsIn(filepath.Join(g.shared, "pids")) {
		waitFor(t, "hung process to end", func() bool { return !alive(p) })
	}
	if left := g.dbFiles(); len(left) != 0 {
		t.Errorf("shard databases left behind: %v", left)
	}
}

func TestFiltersChooseTheTests(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"login1", "login2", "pay1", "broken1"}, 4)
	ran := func(req Request) []string {
		t.Helper()
		req.Suite, req.Heads = "e2e", mainHead()
		run := g.run(req)
		var ids []string
		for id := range last(g.attempts(run.ID)) {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids
	}
	cases := []struct {
		name string
		f    Filter
		want []string
	}{
		{"ids", Filter{IDs: []string{"pay1", "login2"}}, []string{"login2", "pay1"}},
		{"grep", Filter{Grep: "^t/login"}, []string{"login1", "login2"}},
		{"files", Filter{Files: []string{"t/pay1.spec"}}, []string{"pay1"}},
		// The runner lists files relative to its own test dir; people type them from the repo root, or absolute.
		{"file from the repo root", Filter{Files: []string{"e2e/t/pay1.spec"}}, []string{"pay1"}},
		{"absolute file", Filter{Files: []string{"/home/me/shop/e2e/t/pay1.spec"}}, []string{"pay1"}},
		{"directory from the repo root", Filter{Files: []string{"e2e/t/"}}, []string{"broken1", "login1", "login2", "pay1"}},
		{"ids and grep must both match", Filter{IDs: []string{"pay1", "login1"}, Grep: "login"}, []string{"login1"}},
	}
	for _, c := range cases {
		if got := ran(Request{Tests: c.f}); !slices.Equal(got, c.want) {
			t.Errorf("%s: ran %v, want %v", c.name, got, c.want)
		}
	}

	failed := g.run(Request{Suite: "e2e", Heads: mainHead(), Tests: Filter{Grep: "broken|pay"}})
	if failed.State != store.StateFailed {
		t.Fatalf("setup: %s", failed.State)
	}
	if got := ran(Request{Tests: Filter{FailedOf: failed.ID}}); !slices.Equal(got, []string{"broken1"}) {
		t.Errorf("failed-of ran %v, want only broken1", got)
	}
	if _, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead(), Tests: Filter{Grep: "("}}); err == nil {
		t.Error("an invalid grep was accepted")
	}
}

func TestOnTestHooksSeeEveryTestWithItsShard(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1", "pass2", "pass3", "pass4"}, 4)
	var mu sync.Mutex
	begun, ended := map[string]TestCtx{}, map[string]bool{}
	g.r.OnTest(func(c TestCtx) { mu.Lock(); begun[c.Test] = c; mu.Unlock() },
		func(c TestCtx) { mu.Lock(); ended[c.Test] = true; mu.Unlock() })
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))

	mu.Lock()
	defer mu.Unlock()
	if len(begun) != 4 || len(ended) != 4 {
		t.Fatalf("hooks saw %d begins and %d ends, want 4 each", len(begun), len(ended))
	}
	shards := map[int]bool{}
	for _, c := range begun {
		if c.Shard < 1 || c.DB == "" || c.Ports["app"] == 0 || c.Suite != "e2e" {
			t.Errorf("hook context incomplete: %+v", c)
		}
		shards[c.Shard] = true
	}
	if len(shards) != 2 {
		t.Errorf("hooks saw shards %v, want 2 different", shards)
	}
}

func TestHooksEndpointRejectsUnknownRuns(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 4)
	for url, want := range map[string]int{
		g.api.URL + "/runs/nope/hooks/test/begin?test=x":    403, // no run, so no token can match
		g.api.URL + "/runs/nope/hooks/test/sideways?test=x": 403,
	} {
		resp, err := g.api.Client().Post(url, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("POST %s = %d, want %d", url, resp.StatusCode, want)
		}
	}
}

// Capacity: whatever mix of suites and runs is submitted, the admitted shards never need more than the machine
// has, and every run finishes.
func TestCapacityIsNeverExceeded(t *testing.T) {
	names := []string{"a", "b", "c"}
	yaml := "suites:\n"
	for _, n := range names {
		body := strings.TrimPrefix(fakeSuite(""), "suites:\n")
		yaml += strings.Replace(body, "  e2e:", "  "+n+":", 1)
	}
	tests := []string{"pass1", "pass2", "pass3", "pass4", "pass5", "pass6"}
	g := newRig(t, yaml, tests, 3)

	var ids []string
	for round := 0; round < 2; round++ {
		for _, n := range names {
			id, err := g.r.Submit(Request{Suite: n, Heads: mainHead()})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		g.mustPass(g.wait(id))
	}
	st := g.sc.Stats()
	if st.PeakCPUs > 3 || st.PeakCPUs < 2 {
		t.Errorf("peak admitted CPUs = %d, want between 2 and 3", st.PeakCPUs)
	}
	if st.UsedCPUs != 0 || st.Running != 0 || st.Waiting != 0 {
		t.Errorf("capacity not given back: %+v", st)
	}
}

func TestExclusiveSuiteRunsAlone(t *testing.T) {
	yaml := "suites:\n"
	for _, n := range []string{"big", "small"} {
		extra := ""
		if n == "big" {
			extra = "    exclusive: true\n"
		}
		yaml += strings.Replace(strings.TrimPrefix(fakeSuite(extra), "suites:\n"), "  e2e:", "  "+n+":", 1)
	}
	g := newRig(t, yaml, []string{"pass1", "pass2"}, 4)
	var ids []string
	for _, n := range []string{"small", "big", "small"} {
		id, err := g.r.Submit(Request{Suite: n, Heads: mainHead()})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		g.mustPass(g.wait(id))
	}
}

func TestSubmitRejectsWhatItCannotRun(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 2)
	if _, err := g.r.Submit(Request{Suite: "nope"}); err == nil {
		t.Error("unknown suite accepted")
	}
	if _, err := g.r.Submit(Request{Suite: "e2e", Heads: []Head{{Head: prHead(1)[0].Head}, {}}}); err == nil {
		t.Error("a head for a repository outside the suite was accepted")
	}
}

func TestReportLineCannotForgeAnSSEFrame(t *testing.T) {
	t.Setenv("FAKE_RAW", `{"v":1,"t":"x\nid: 999999999","ts":1}`)
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)
	evs, err := g.st.RunEvents(context.Background(), run.ID)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	kept := false
	for _, e := range evs {
		if !store.ValidKind(e.Kind) {
			t.Errorf("event %d has kind %q", e.Seq, e.Kind)
		}
		kept = kept || strings.Contains(string(e.Data), "999999999")
	}
	if !kept {
		t.Error("the odd line was dropped instead of kept as data")
	}
}

// A framework's report cannot pass itself off as localci's own state events
// (the dashboard, statuses and restarts read run.state).
func TestReportLineCannotSpoofACoreEvent(t *testing.T) {
	t.Setenv("FAKE_RAW", `{"v":1,"t":"run.state","ts":1,"state":"passed","spoof":"yes"}`)
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)
	evs, _ := g.st.RunEvents(context.Background(), run.ID)
	kept := false
	for _, e := range evs {
		spoof := strings.Contains(string(e.Data), `"spoof"`)
		if spoof && e.Kind != "report.invalid" {
			t.Errorf("report line stored as a %s event", e.Kind)
		}
		kept = kept || spoof
	}
	if !kept {
		t.Error("the line was dropped instead of kept as data")
	}
}

func TestUnreadableReportLinesAreCountedAndNeverPass(t *testing.T) {
	t.Setenv("FAKE_BAD_END", "1")
	g := newRig(t, fakeSuite(""), []string{"pass1", "pass2"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "report lines could not be read") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	for id, a := range last(g.attempts(run.ID)) {
		if a.Status == "passed" {
			t.Errorf("%s passed on lines nobody could read", id)
		}
	}
}

func TestUnreadableLineIsNotedOnAPassingRun(t *testing.T) {
	t.Setenv("FAKE_RAW", `{"v":1,"t":"step.end","step":5,"ms":1,"ok":true}`)
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)
	if !strings.Contains(run.Reason, "1 report lines could not be read") {
		t.Errorf("reason = %q", run.Reason)
	}
}

func TestUnsupportedReportVersionIsARunError(t *testing.T) {
	t.Setenv("FAKE_FIRST", `{"v":2,"t":"begin","ts":1,"shard":1,"total":1}`)
	g := newRig(t, fakeSuite(""), []string{"pass1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "version") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
}

func TestShardThatExitsWithoutTheEndEventFails(t *testing.T) {
	t.Setenv("FAKE_NO_END", "1")
	g := newRig(t, fakeSuite(""), []string{"pass1", "pass2"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "no complete report") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
}

func TestListedTestThatIsNeverReportedIsInterrupted(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1", "ghost1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
	got := last(g.attempts(run.ID))
	if got["ghost1"].Status != "interrupted" || got["pass1"].Status != "passed" {
		t.Errorf("statuses = %+v", got)
	}
}

func TestListingNothingWithoutAFilterFails(t *testing.T) {
	g := newRig(t, fakeSuite(""), nil, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || !strings.Contains(run.Reason, "no tests") {
		t.Fatalf("run = %s (%s)", run.State, run.Reason)
	}
}

func TestCancelledInvocationLeavesNoTestRunning(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"hang1"}, 4)
	id, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead()})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the test to start", func() bool { return len(pidsIn(filepath.Join(g.shared, "pids"))) > 0 })
	if !g.r.Cancel(id) {
		t.Fatal("run is not active")
	}
	g.wait(id)
	for tid, a := range last(g.attempts(id)) {
		if a.Status == "running" {
			t.Errorf("%s is still running in a finished run", tid)
		}
	}
}

func TestTimeoutDoesNotCountTimeSpentWaitingForCapacity(t *testing.T) {
	yaml := ""
	for _, n := range []string{"big", "quick"} {
		extra := "    exclusive: true\n"
		if n == "quick" {
			extra = "    timeout: 3s\n"
		}
		yaml += strings.Replace(strings.TrimPrefix(fakeSuite(extra), "suites:\n"), "  e2e:", "  "+n+":", 1)
	}
	g := newRig(t, "suites:\n"+yaml, []string{"slow1", "pass1"}, 4)
	bigID, err := g.r.Submit(Request{Suite: "big", Heads: mainHead(), Tests: Filter{IDs: []string{"slow1"}}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the exclusive run to start", func() bool {
		run, err := g.st.Run(context.Background(), bigID)
		return err == nil && run.State == store.StateRunning
	})
	// quick now waits for the machine for longer than its own timeout, and still has to run its test.
	g.mustPass(g.run(Request{Suite: "quick", Heads: mainHead(), Tests: Filter{IDs: []string{"pass1"}}}))
	g.mustPass(g.wait(bigID))
}

// A run's hook token opens that run's hooks only, and only while it runs: a token that leaked from a finished run,
// or one from another run, drives nothing.
func TestHookTokenIsPerRunAndEndsWithTheRun(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"pass1", "pass2"}, 4)
	tokens := map[string]string{}
	var mu sync.Mutex
	g.r.OnTest(func(c TestCtx) {
		mu.Lock()
		defer mu.Unlock()
		if _, seen := tokens[c.Run]; !seen {
			g.r.mu.Lock()
			tokens[c.Run] = g.r.live[c.Run].hookToken
			g.r.mu.Unlock()
		}
	}, nil)
	first := g.run(Request{Suite: "e2e", Heads: mainHead()})
	second := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(first)
	g.mustPass(second)
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[first.ID] == tokens[second.ID] || tokens[first.ID] == "" {
		t.Fatalf("each run needs its own token, got %v", tokens)
	}
	if g.r.HookTokenValid(first.ID, tokens[first.ID]) {
		t.Error("the token of a finished run still opens its hooks")
	}
}

func TestHookTokenOfAnotherRunIsRefused(t *testing.T) {
	r := &Runner{live: map[string]*execution{
		"1": {hookToken: "aaaa"},
		"2": {hookToken: "bbbb"},
	}}
	for _, c := range []struct {
		run, token string
		want       bool
	}{{"1", "aaaa", true}, {"1", "bbbb", false}, {"2", "aaaa", false}, {"1", "", false}, {"3", "aaaa", false}} {
		if got := r.HookTokenValid(c.run, c.token); got != c.want {
			t.Errorf("run %s token %q: %v, want %v", c.run, c.token, got, c.want)
		}
	}
}

// A fork run executes code nobody reviewed, and its report is written by that code: it may run and report, but it
// teaches the planning of later runs nothing (serial group, write sets, durations).
func TestForkRunTeachesLaterRunsNothing(t *testing.T) {
	t.Setenv("FAKE_RAW", `{"v":1,"t":"writes","test":"pass1","tables":["orders"]}`)
	g := newRig(t, fakeSuite("    isolation: auto\n    writes: report\n"), []string{"setter1", "reader1", "pass1"}, 4)
	ctx := context.Background()
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead(), Fork: true}))

	if serial, _ := g.st.Serial(ctx, "e2e"); len(serial) != 0 {
		t.Errorf("a fork run put tests in the serial group: %+v", serial)
	}
	if writes, _ := g.st.Writes(ctx, "e2e"); len(writes) != 0 {
		t.Errorf("a fork run stored write sets: %+v", writes)
	}
	if d, _ := g.st.MedianDurations(ctx, []string{"setter1", "reader1", "pass1"}); len(d) != 0 {
		t.Errorf("a fork run recorded durations: %v", d)
	}
}

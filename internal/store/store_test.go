package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func putRun(t *testing.T, s *Store, id, state string, created int64) {
	t.Helper()
	if err := s.PutRun(context.Background(), Run{ID: id, Suite: "web", Kind: KindSuite, State: state, Created: created}); err != nil {
		t.Fatal(err)
	}
}

// A crash leaves runs preparing or running: after reopening the file they go back to the queue, finished ones stay.
func TestRecoverStaleAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	putRun(t, s, "a", StateRunning, 1)
	putRun(t, s, "b", StatePassed, 2)
	putRun(t, s, "c", StateQueued, 3)
	putRun(t, s, "d", StatePreparing, 4)
	s.Close() // no clean shutdown of the runs

	s = open(t, path)
	got, err := s.RecoverStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "d" {
		t.Fatalf("recovered %+v, want runs a and d", got)
	}
	for id, want := range map[string]string{"a": StateQueued, "b": StatePassed, "c": StateQueued, "d": StateQueued} {
		if r, _ := s.Run(ctx, id); r.State != want {
			t.Errorf("run %s is %s, want %s", id, r.State, want)
		}
	}
	if again, _ := s.RecoverStale(ctx); len(again) != 0 {
		t.Fatalf("second recovery found %d runs", len(again))
	}
}

func TestRetentionKeepsNewest1000WithTheirData(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	const total = Retention + 25
	for i := 1; i <= total; i++ {
		id := fmt.Sprintf("r%04d", i)
		putRun(t, s, id, StatePassed, int64(i))
		s.AppendEvent(ctx, Event{Kind: "run.passed", Run: id})
		s.PutTest(ctx, Test{Run: id, ID: "t", Status: "passed"})
		s.PutStep(ctx, Step{Run: id, Test: "t", ID: "s1"})
		s.PutArtifact(ctx, Artifact{Run: id, Kind: "trace", Path: "/x"})
	}
	s.AppendEvent(ctx, Event{Kind: "worker.joined"}) // not tied to a run

	runs, _ := s.Runs(ctx, 0)
	if len(runs) != Retention || runs[0].ID != fmt.Sprintf("r%04d", total) || runs[Retention-1].ID != "r0026" {
		t.Fatalf("kept %d runs, first %s last %s", len(runs), runs[0].ID, runs[len(runs)-1].ID)
	}
	if ev, _ := s.RunEvents(ctx, "r0025"); len(ev) != 0 {
		t.Fatal("events of a dropped run survived")
	}
	if ev, _ := s.RunEvents(ctx, "r0026"); len(ev) != 1 {
		t.Fatal("events of a kept run were dropped")
	}
	var orphans int
	for _, q := range []string{
		`SELECT COUNT(*) FROM tests WHERE run NOT IN (SELECT id FROM runs)`,
		`SELECT COUNT(*) FROM steps WHERE run NOT IN (SELECT id FROM runs)`,
		`SELECT COUNT(*) FROM artifacts WHERE run NOT IN (SELECT id FROM runs)`,
		`SELECT COUNT(*) FROM events WHERE run <> '' AND run NOT IN (SELECT id FROM runs)`,
	} {
		var n int
		s.r.QueryRow(q).Scan(&n)
		orphans += n
	}
	if orphans != 0 {
		t.Fatalf("%d rows left behind by dropped runs", orphans)
	}
	if last, _ := s.LastSeq(ctx); last != int64(total)+1 {
		t.Fatalf("last seq %d: run-less events must survive and seq must not be reused", last)
	}
}

func TestRetentionNeverDropsActiveRuns(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	putRun(t, s, "old-active", StateRunning, 1)
	for i := 2; i <= Retention+5; i++ {
		putRun(t, s, fmt.Sprintf("r%d", i), StatePassed, int64(i))
	}
	if _, err := s.Run(ctx, "old-active"); err != nil {
		t.Fatalf("an active run was pruned: %v", err)
	}
}

func TestTimelineGroupsStepsByShardLane(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	if err := s.PutRun(ctx, Run{ID: "r", Suite: "web", Kind: KindSuite, State: StateRunning, Created: 900, Started: 1000}); err != nil {
		t.Fatal(err)
	}
	// shard 2 has a retried test: the lane comes from the latest attempt.
	for _, tt := range []Test{
		{Run: "r", Shard: 1, ID: "a", Status: "passed"},
		{Run: "r", Shard: 2, ID: "b", Status: "failed", Retry: 0},
		{Run: "r", Shard: 2, ID: "b", Status: "passed", Retry: 1},
	} {
		if err := s.PutTest(ctx, tt); err != nil {
			t.Fatal(err)
		}
	}
	yes, no := true, false
	for _, st := range []Step{
		{Run: "r", Test: "b", ID: "b1", Title: "open", Kind: "step", Start: 1100, MS: 50, OK: &yes},
		{Run: "r", Test: "a", ID: "a2", Parent: "a1", Title: "click", Kind: "step", Start: 1250, MS: 30, OK: &no},
		{Run: "r", Test: "a", ID: "a1", Title: "login", Kind: "flow", Start: 1200, MS: 120, OK: &yes},
	} {
		if err := s.PutStep(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	// still running: not ended yet.
	s.PutStep(ctx, Step{Run: "r", Test: "b", ID: "b2", Title: "wait", Kind: "step", Start: 1300})
	s.EndStep(ctx, "r", "b", "b2", 40, true)
	s.PutStep(ctx, Step{Run: "r", Test: "b", ID: "b3", Title: "open-ended", Kind: "step", Start: 1400})

	lanes, err := s.Timeline(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		id  string
		off int64
	}
	want := map[int][]step{1: {{"a1", 200}, {"a2", 250}}, 2: {{"b1", 100}, {"b2", 300}, {"b3", 400}}}
	if len(lanes) != 2 || lanes[0].Shard != 1 || lanes[1].Shard != 2 {
		t.Fatalf("lanes %+v", lanes)
	}
	for _, l := range lanes {
		var got []step
		for _, st := range l.Steps {
			got = append(got, step{st.ID, st.Offset})
		}
		if fmt.Sprint(got) != fmt.Sprint(want[l.Shard]) {
			t.Errorf("shard %d: got %v, want %v", l.Shard, got, want[l.Shard])
		}
	}
	if b3 := lanes[1].Steps[2]; b3.OK != nil || b3.MS != 0 {
		t.Errorf("unfinished step reported %+v", b3)
	}
	if a2 := lanes[0].Steps[1]; a2.OK == nil || *a2.OK || a2.Parent != "a1" {
		t.Errorf("failed step lost its outcome or parent: %+v", a2)
	}
}

func TestFailuresIgnoreTestsThatPassedOnRetry(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	fail := json.RawMessage(`{"step":"click","message":"boom"}`)
	for _, tt := range []Test{
		{Run: "r", Shard: 1, ID: "flaky", Status: "failed", Retry: 0, Failure: fail},
		{Run: "r", Shard: 1, ID: "flaky", Status: "passed", Retry: 1},
		{Run: "r", Shard: 1, ID: "broken", Status: "failed", Retry: 0, Failure: fail},
		{Run: "r", Shard: 1, ID: "broken", Status: "failed", Retry: 1, Failure: fail},
		{Run: "r", Shard: 2, ID: "slow", Status: "timedOut", Failure: fail},
		{Run: "r", Shard: 2, ID: "fine", Status: "passed"},
	} {
		s.PutTest(ctx, tt)
	}
	got, err := s.Failures(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range got {
		ids = append(ids, fmt.Sprintf("%s/%d", f.ID, f.Retry))
	}
	if fmt.Sprint(ids) != "[broken/1 slow/0]" {
		t.Fatalf("failures %v, want [broken/1 slow/0]", ids)
	}
	if string(got[0].Failure) != string(fail) {
		t.Fatalf("failure payload %s", got[0].Failure)
	}
}

func TestMedianDurationsUseLast10(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, ms := range []int64{9000, 9000} { // old outliers, pushed out by 10 newer runs
		s.RecordDuration(ctx, "t1", ms)
	}
	for ms := int64(100); ms <= 1000; ms += 100 {
		s.RecordDuration(ctx, "t1", ms)
	}
	s.RecordDuration(ctx, "t2", 70)
	got, err := s.MedianDurations(ctx, []string{"t1", "t2", "never"})
	if err != nil {
		t.Fatal(err)
	}
	if got["t1"] != 550 || got["t2"] != 70 {
		t.Fatalf("medians %v, want t1=550 t2=70", got)
	}
	if _, ok := got["never"]; ok {
		t.Fatal("unmeasured test has a median")
	}
}

func TestSerialGroupPerSuite(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	ev := json.RawMessage(`{"overlaps":["b"]}`)
	s.AddSerial(ctx, SerialEntry{Suite: "web", Test: "a", Evidence: ev, Since: 5})
	s.AddSerial(ctx, SerialEntry{Suite: "web", Test: "a", Evidence: ev, Since: 99}) // again: keeps the first Since
	s.AddSerial(ctx, SerialEntry{Suite: "api", Test: "z"})
	got, _ := s.Serial(ctx, "web")
	if len(got) != 1 || got[0].Test != "a" || got[0].Since != 5 || string(got[0].Evidence) != string(ev) {
		t.Fatalf("serial web: %+v", got)
	}
	s.RemoveSerial(ctx, "web", "a")
	if got, _ := s.Serial(ctx, "web"); len(got) != 0 {
		t.Fatalf("still serial after removal: %+v", got)
	}
}

func TestRunsSinceCursorAndStateStamps(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	putRun(t, s, "r1", StateQueued, 1)
	putRun(t, s, "r2", StateQueued, 2)
	s.AppendEvent(ctx, Event{Kind: "run.queued", Run: "r1"})
	cur, _ := s.AppendEvent(ctx, Event{Kind: "run.queued", Run: "r2"})
	s.AppendEvent(ctx, Event{Kind: "run.running", Run: "r1"})

	got, _ := s.Runs(ctx, cur)
	if len(got) != 1 || got[0].ID != "r1" {
		t.Fatalf("runs since %d: %+v, want only r1", cur, got)
	}
	if err := s.SetRunState(ctx, "r1", StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRunState(ctx, "r1", StateFailed, "tests failed"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Run(ctx, "r1")
	if r.State != StateFailed || r.Reason != "tests failed" || r.Started == 0 || r.Finished == 0 {
		t.Fatalf("run after lifecycle: %+v", r)
	}
	if err := s.SetRunState(ctx, "missing", StateFailed, ""); err != ErrNotFound {
		t.Fatalf("missing run: %v", err)
	}
}

func TestAppendEventRefusesKindsThatBreakSSEFraming(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "s.db"))
	for _, k := range []string{"", "x\nid: 999999999", "a\rb", "has space", "data: x", string(make([]byte, 65))} {
		if _, err := s.AppendEvent(context.Background(), Event{Kind: k, Run: "r"}); err == nil {
			t.Errorf("kind %q was accepted", k)
		}
	}
	if _, err := s.AppendEvent(context.Background(), Event{Kind: "test.end-2_x", Run: "r"}); err != nil {
		t.Errorf("a plain kind was refused: %v", err)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// A data directory an earlier version made 0755, with a 0644 database and token, is private after Harden and
// Open; the WAL and shared-memory files follow the database.
func TestHardenMakesAnOldDataDirPrivate(t *testing.T) {
	data := filepath.Join(t.TempDir(), "localci")
	for _, d := range []string{"", "logs", "checkpoints/web"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(filepath.Join(data, d), 0o755)
	}
	os.Chmod(data, 0o755)
	os.WriteFile(filepath.Join(data, "token"), []byte("t\n"), 0o644)
	os.WriteFile(filepath.Join(data, "localci.db"), nil, 0o644)

	if loose, _ := LooseModes(data); len(loose) != 6 {
		t.Fatalf("setup: %d loose paths, want 6: %v", len(loose), loose)
	}
	if err := Harden(data); err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(data, "localci.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutRun(t.Context(), Run{ID: "1", Suite: "e2e", Kind: KindSuite, State: StateQueued}); err != nil {
		t.Fatal(err)
	}
	if loose, err := LooseModes(data); err != nil || len(loose) != 0 {
		t.Errorf("still loose after Harden and Open: %v %v", loose, err)
	}
	for _, f := range []string{"localci.db", "localci.db-wal", "localci.db-shm"} {
		if m := mode(t, filepath.Join(data, f)); m != 0o600 {
			t.Errorf("%s is %o, want 600", f, m)
		}
	}
	if m := mode(t, data); m != 0o700 {
		t.Errorf("data dir is %o, want 700", m)
	}
}

// A branch run can add tables to a test's set but never remove one; a run on the default branch replaces the set
// and is the only one that makes it trusted.
func TestPutWritesBranchRunsOnlyAddAndDefaultBranchReplaces(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	put := func(w WriteSet) WriteSet {
		t.Helper()
		w.Suite = "web"
		if err := s.PutWrites(ctx, w); err != nil {
			t.Fatal(err)
		}
		got, err := s.Writes(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		return got[w.Test]
	}
	a := put(WriteSet{Test: "a", Tables: []string{"orders"}, Reads: []string{"orders"}})
	if a.Trusted {
		t.Fatal("a branch run must not make a set trusted")
	}
	a = put(WriteSet{Test: "a", Tables: []string{"audit"}, Reads: []string{"users"}})
	if !slices.Equal(a.Tables, []string{"audit", "orders"}) || !slices.Equal(a.Reads, []string{"orders", "users"}) || a.Trusted {
		t.Fatalf("branch runs add: %+v", a)
	}
	a = put(WriteSet{Test: "a", Tables: []string{"orders"}, Reads: []string{"orders"}, Replace: true})
	if !slices.Equal(a.Tables, []string{"orders"}) || !slices.Equal(a.Reads, []string{"orders"}) || !a.Trusted {
		t.Fatalf("a default-branch run replaces and trusts: %+v", a)
	}
	a = put(WriteSet{Test: "a", Tables: []string{"billing"}})
	if !slices.Equal(a.Tables, []string{"billing", "orders"}) || !a.Trusted {
		t.Fatalf("a branch run after it adds and leaves the mark: %+v", a)
	}
	if b := put(WriteSet{Test: "b"}); b.Test != "b" || len(b.Tables) != 0 {
		t.Fatalf("an empty measured set is still known: %+v", b)
	}
}

// A store made by the release before the trusted mark upgrades in place and its sets start untrusted.
func TestStoreWithoutTheTrustedMarkUpgradesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if _, err := raw.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`PRAGMA user_version = 4; INSERT INTO writes(suite,test,tables,reads,ts) VALUES('web','a','["orders"]','[]',1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s := open(t, path)
	got, err := s.Writes(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if a := got["a"]; !slices.Equal(a.Tables, []string{"orders"}) || a.Trusted {
		t.Fatalf("a = %+v: the old set is kept and untrusted", a)
	}
}

// A fork run's report is not trusted: its tests never make the flaky list that triage reads.
func TestFlakyListIgnoresForkRuns(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, r := range []Run{{ID: "1", Suite: "web", Kind: KindSuite, State: StatePassed, Created: 10},
		{ID: "2", Suite: "web", Kind: KindSuite, State: StatePassed, Created: 20, Fork: true}} {
		if err := s.PutRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	s.PutTest(ctx, Test{Run: "1", ID: "cart", Status: "passed", Class: "flaky"})
	s.PutTest(ctx, Test{Run: "2", ID: "login", Status: "passed", Class: "flaky"})
	got, err := s.Flaky(ctx, 0)
	if err != nil || len(got) != 1 || got[0].Test != "cart" {
		t.Fatalf("flaky = %+v, %v; want only cart", got, err)
	}
}

// A store a release before the infra column upgrades in place: its runs stay, they are not infrastructure
// failures, and the column works.
func TestStoreFromBeforeInfraUpgradesInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := openDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:4] { // what main had before migration 5
		if _, err := old.Exec(m); err != nil {
			t.Fatal(err)
		}
		old.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1))
	}
	if _, err := old.Exec(`INSERT INTO runs(id,suite,kind,state,created) VALUES('1','web','suite','failed',5)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s := open(t, path)
	r, err := s.Run(ctx, "1")
	if err != nil || r.State != "failed" || r.Infra != "" {
		t.Fatalf("run after the upgrade = %+v, %v", r, err)
	}
	if first, err := s.SetRunInfra(ctx, "1", "stack app exited"); err != nil || !first {
		t.Fatalf("SetRunInfra = %v, %v", first, err)
	}
	if first, _ := s.SetRunInfra(ctx, "1", "later reason"); first {
		t.Fatal("the first reason must stay")
	}
	if r, _ := s.Run(ctx, "1"); r.Infra != "stack app exited" {
		t.Fatalf("infra = %q", r.Infra)
	}
}

// Tests the stack never let finish are not failed tests: an infrastructure run counts them in the total only,
// while an ordinary run counts an interrupted test (a shard that crashed) as failed. A run whose stack broke
// never feeds the flaky list.
func TestInterruptedTestsOfAnInfrastructureRunAreNotFailures(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "state.db"))
	for _, r := range []Run{{ID: "1", Suite: "web", Kind: KindSuite, State: StateFailed, Created: 10},
		{ID: "2", Suite: "web", Kind: KindSuite, State: StateFailed, Created: 20, Infra: "stack app exited"}} {
		s.PutRun(ctx, r)
		s.PutTest(ctx, Test{Run: r.ID, ID: "a", Status: "passed"})
		s.PutTest(ctx, Test{Run: r.ID, ID: "b", Status: "interrupted"})
		s.PutTest(ctx, Test{Run: r.ID, ID: "c", Status: "failed"})
		s.PutTest(ctx, Test{Run: r.ID, ID: "d", Status: "passed", Class: "flaky"})
	}
	if c, _ := s.TestCounts(ctx, "1"); c != (Counts{Total: 4, Passed: 2, Failed: 2}) {
		t.Fatalf("ordinary run counts = %+v", c)
	}
	if c, _ := s.TestCounts(ctx, "2"); c != (Counts{Total: 4, Passed: 2, Failed: 1}) {
		t.Fatalf("infrastructure run counts = %+v, want the interrupted test out of Failed", c)
	}
	if got, _ := s.Flaky(ctx, 0); len(got) != 1 {
		t.Fatalf("flaky = %+v, want d from the ordinary run only", got)
	}
}

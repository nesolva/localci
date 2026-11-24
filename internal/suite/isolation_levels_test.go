package suite

import (
	"context"
	"encoding/json"
	"fmt"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

func TestIsolationKeysAreChecked(t *testing.T) {
	base := "suites:\n  e2e:\n    repos: [acme/web]\n    test: {repo: acme/web, run: make e2e}\n"
	db := "    database: {driver: postgres, url: 'postgresql://x@h/postgres', template: {repo: acme/web, seed: make seed}}\n"
	for _, c := range []struct{ yaml, want string }{
		{base + "    isolation: shared-db\n", "needs a database"},
		{base + db + "    writes: guess\n", "use counters or report"},
		{base + db + "    isolation: tenant\n", "needs isolation_tenant_flow"},
		{base + db + "    isolation: tenant\n    isolation_tenant_flow: make-tenant\n", "needs a stack"},
		{base + db + "    isolation: tenant\n    isolation_tenant_flow: 'a b'\n", "is not a flow name"},
		{base + db + "    writes: {schema: Bad-Name}\n", "writes.schema"},
		{base + db + "    writes: {mode: guess}\n", "use counters or report"},
	} {
		if _, err := ParseYAML([]byte(c.yaml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want an error with %q, got %v", c.want, err)
		}
	}
	for _, level := range []string{"auto", "shared-db"} {
		ss, err := ParseYAML([]byte(base + db + "    isolation: " + level + "\n    writes: report\n"))
		if err != nil || ss[0].Isolation != level {
			t.Errorf("isolation %s: %v", level, err)
		}
	}
}

// A report is written by the code under test: its writes line decides nothing unless the suite says the engine
// cannot be measured and the report is the source.
func TestWritesLineCountsOnlyWhenTheSuiteSaysReport(t *testing.T) {
	t.Setenv("FAKE_RAW", `{"v":1,"t":"writes","test":"pass1","tables":["orders","audit"]}`)
	for mode, want := range map[string][]string{"counters": nil, "report": {"audit", "orders"}} {
		g := newRig(t, fakeSuite("    isolation: auto\n    writes: "+mode+"\n"), []string{"pass1"}, 4)
		g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))
		got, err := g.st.Writes(context.Background(), "e2e")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got["pass1"].Tables, want) {
			t.Errorf("writes: %s: stored %v, want %v", mode, got["pass1"].Tables, want)
		}
	}
}

// A set is stored only for an attempt that passed, whichever of the measurement and the report's verdict comes
// first; what a failed, skipped or interrupted attempt touched says nothing about the product.
func TestWritesStoredOnlyForPassedTests(t *testing.T) {
	g := newRig(t, fakeSuite("    isolation: auto\n"), []string{"pass1"}, 4)
	ctx := context.Background()
	ex := &execution{r: g.r, suite: g.r.suites["e2e"], id: "7", ctx: ctx, bg: ctx, iso: &isoRun{alone: map[string][]string{}}}
	for _, c := range []struct {
		test, status string
		verdictFirst bool
	}{
		{"passed", "passed", false}, {"passed-late", "passed", true}, {"failed", "failed", false},
		{"skipped", "skipped", true}, {"interrupted", "interrupted", false}, {"timedout", "timedOut", true},
	} {
		a := shard.Access{Writes: []string{"orders"}}
		if c.verdictFirst {
			ex.testOutcome(c.test, c.status)
			ex.learned(1, c.test, a)
		} else {
			ex.learned(1, c.test, a)
			ex.testOutcome(c.test, c.status)
		}
	}
	got, err := g.st.Writes(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for id := range got {
		stored = append(stored, id)
	}
	slices.Sort(stored)
	if !slices.Equal(stored, []string{"passed", "passed-late"}) {
		t.Fatalf("stored sets for %v, want only the passed ones", stored)
	}
}

// A run on every repository's default branch replaces a test's set and trusts it; a run on a branch adds tables and
// cannot remove one.
func TestWritesBranchRunAddsAndDefaultBranchRunReplaces(t *testing.T) {
	g := newRig(t, fakeSuite("    isolation: auto\n    writes: report\n"), []string{"pass1"}, 4)
	git(t, g.src, "checkout", "-q", "-b", "feature")
	g.commit(map[string]string{"feature.txt": "x\n"})
	git(t, g.src, "checkout", "-q", "main")
	feature := []Head{{Head: forge.Head{Repo: "acme/web", Branch: "feature"}}}

	learn := func(heads []Head, tables string) store.WriteSet {
		t.Helper()
		t.Setenv("FAKE_RAW", `{"v":1,"t":"writes","test":"pass1","tables":[`+tables+`]}`)
		g.mustPass(g.run(Request{Suite: "e2e", Heads: heads}))
		got, err := g.st.Writes(context.Background(), "e2e")
		if err != nil {
			t.Fatal(err)
		}
		return got["pass1"]
	}
	if w := learn(feature, `"a"`); !slices.Equal(w.Tables, []string{"a"}) || w.Trusted {
		t.Fatalf("a branch run teaches a set but does not trust it: %+v", w)
	}
	if w := learn(mainHead(), `"b"`); !slices.Equal(w.Tables, []string{"b"}) || !w.Trusted {
		t.Fatalf("the default branch replaces and trusts: %+v", w)
	}
	if w := learn(feature, `"c"`); !slices.Equal(w.Tables, []string{"b", "c"}) || !w.Trusted {
		t.Fatalf("a branch run adds and removes nothing: %+v", w)
	}
}

func TestWritesSchemaIsSetUnderWrites(t *testing.T) {
	base := "suites:\n  e2e:\n    repos: [acme/web]\n    test: {repo: acme/web, run: make e2e}\n" +
		"    database: {driver: postgres, url: 'postgresql://x@h/postgres', template: {repo: acme/web, seed: make seed}}\n"
	for yaml, want := range map[string][2]string{
		base:                                  {"counters", "localci"},
		base + "    writes: report\n":         {"report", "localci"},
		base + "    writes: {schema: wlog}\n": {"counters", "wlog"},
		base + "    writes: {mode: report, schema: wlog}\n": {"report", "wlog"},
	} {
		ss, err := ParseYAML([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		if got := [2]string{writesMode(ss[0]), writesSchema(ss[0])}; got != want {
			t.Errorf("%q: mode and schema = %v, want %v", yaml, got, want)
		}
	}
}

func isolationEvent(t *testing.T, g *rig, run string) map[string]any {
	t.Helper()
	evs, err := g.st.RunEvents(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == "run.isolation" {
			var m map[string]any
			_ = json.Unmarshal(e.Data, &m)
			return m
		}
	}
	return nil
}

// With known, disjoint write sets the shards share one database, which the run drops afterwards.
func TestSharedDBShardsUseOneDatabaseAndTheRunDropsIt(t *testing.T) {
	ids := []string{"pass1", "pass2", "pass3", "pass4"}
	g := newRig(t, fakeSuite("    isolation: shared-db\n    writes: report\n"), ids, 4)
	for _, id := range ids {
		if err := g.st.PutWrites(context.Background(), store.WriteSet{Suite: "e2e", Test: id, Tables: []string{"t_" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)

	if ev := isolationEvent(t, g, run.ID); ev == nil || ev["level"] != "shared-db" || ev["shared_shards"] != float64(2) {
		t.Fatalf("run.isolation = %v, want shared-db on 2 shards", ev)
	}
	dbs := map[string]bool{}
	for _, l := range readLines(filepath.Join(g.shared, "shards.log")) {
		dbs[strings.Fields(l)[1]] = true
	}
	if len(dbs) != 1 {
		t.Fatalf("shards used %d databases, want the one shared: %v", len(dbs), dbs)
	}
	for name := range dbs {
		if !strings.HasSuffix(name, "_s") {
			t.Fatalf("database %q is not the shared one", name)
		}
	}
	if left := g.dbFiles(); len(left) != 0 {
		t.Fatalf("databases left after the run: %v", left)
	}
}

// The Docker VM's memory limits how many whole stacks fit; `auto` knows it, and shares a database only when every
// test's set was last replaced on the default branch.
func TestAutoWeighsDockerMemoryAndTrustedSets(t *testing.T) {
	ids := []string{"pass1", "pass2", "pass3", "pass4", "pass5", "pass6"}
	for _, c := range []struct {
		name      string
		untrusted bool
		level     string
	}{{"every set trusted", false, "shared-db"}, {"one set learned on a branch", true, "isolated"}} {
		auto := strings.Replace(fakeSuite("    isolation: auto\n    writes: report\n"), "shards: 2", "shards: auto", 1)
		g := newRig(t, auto, ids, 8)
		g.sc.WithDockerMemory(3 << 30) // two stacks at the default cost
		for i, id := range ids {
			w := store.WriteSet{Suite: "e2e", Test: id, Tables: []string{"t_" + id}, Replace: !(c.untrusted && i == 0)}
			if err := g.st.PutWrites(context.Background(), w); err != nil {
				t.Fatal(err)
			}
		}
		run := g.run(Request{Suite: "e2e", Heads: mainHead()})
		g.mustPass(run)
		if ev := isolationEvent(t, g, run.ID); ev == nil || ev["level"] != c.level {
			t.Errorf("%s: run.isolation = %v, want %s", c.name, ev, c.level)
		}
	}
}

// Nothing is known on the first run, so auto keeps every shard on its own database.
func TestAutoStartsIsolatedUntilWriteSetsAreKnown(t *testing.T) {
	g := newRig(t, fakeSuite("    isolation: auto\n    writes: report\n"), []string{"pass1", "pass2"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)
	if ev := isolationEvent(t, g, run.ID); ev == nil || ev["level"] != "isolated" {
		t.Fatalf("run.isolation = %v, want isolated", ev)
	}
}

// A suspect that wrote the unexplained table when run alone joins the serial group; one that did not, or was never
// seen alone, is cleared and the run's failures stay what the tests themselves reported.
func TestSuspectsAreConfirmedByWhatTheyWroteAlone(t *testing.T) {
	g := newRig(t, fakeSuite("    isolation: auto\n"), []string{"pass1"}, 4)
	ctx := context.Background()
	ex := &execution{r: g.r, suite: g.r.suites["e2e"], id: "7", ctx: ctx, bg: ctx,
		iso: &isoRun{alone: map[string][]string{"a": {"ledger", "orders"}, "b": {"audit"}}}}
	ex.iso.suspects = []shard.Suspect{
		{Test: "a", Unexplained: []string{"ledger"}, Overlap: []shard.TestID{"b", "c"}},
		{Test: "b", Unexplained: []string{"ledger"}, Overlap: []shard.TestID{"a", "c"}},
		{Test: "c", Unexplained: []string{"ledger"}, Overlap: []shard.TestID{"a", "b"}}, // never run alone
	}
	if err := ex.confirmSuspects(); err != nil {
		t.Fatal(err)
	}
	entries, _ := g.st.Serial(ctx, "e2e")
	if len(entries) != 1 || entries[0].Test != "a" {
		t.Fatalf("serial group = %+v, want only a", entries)
	}
	if !strings.Contains(string(entries[0].Evidence), "ledger") || !strings.Contains(string(entries[0].Evidence), `"with"`) {
		t.Fatalf("evidence = %s", entries[0].Evidence)
	}
	if ex.movedCount() != 1 {
		t.Fatalf("moved = %d", ex.movedCount())
	}
	if got := ex.probes([]shard.TestID{"a"}); !slices.Equal(got, []shard.TestID{"b", "c"}) {
		t.Fatalf("probes = %v: suspects that did not fail in parallel are only measured alone", got)
	}
}

// startTestPostgres runs a throwaway Postgres in Docker and skips the test without Docker.
func startTestPostgres(t *testing.T) *shard.Postgres {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if err := osexec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	name := fmt.Sprintf("localci-test-h-%d", time.Now().UnixNano())
	if out, err := osexec.Command("docker", "run", "-d", "--rm", "--name", name, "-e", "POSTGRES_PASSWORD=test", "postgres:17").CombinedOutput(); err != nil {
		t.Skipf("cannot start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = osexec.Command("docker", "rm", "-f", name).Run() })
	pg := &shard.Postgres{Container: name}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		if osexec.Command("docker", "exec", name, "pg_isready", "-h", "127.0.0.1").Run() == nil && pg.Exec(context.Background(), "postgres", "select 1") == nil {
			return pg
		}
		if time.Now().After(deadline) {
			t.Fatal("postgres did not become ready")
		}
	}
}

// The hooks against a real Postgres: a test alone in its database teaches its write set; in the shared database
// two overlapping tests that write a table neither is known to write both become suspects.
func TestHooksMeasureWritesInPostgres(t *testing.T) {
	pg := startTestPostgres(t)
	ctx := context.Background()
	for _, db := range []string{"localci_iso", "localci_iso_s"} {
		if err := pg.Exec(ctx, "postgres", "create database "+db); err != nil {
			t.Fatal(err)
		}
		if err := pg.Exec(ctx, db, "create table a(v int); create table b(v int); create table x(v int)"); err != nil {
			t.Fatal(err)
		}
	}

	g := newRig(t, fakeSuite("    isolation: auto\n"), []string{"pass1"}, 4)
	ex := &execution{r: g.r, suite: g.r.suites["e2e"], id: "9", ctx: ctx, bg: ctx,
		db: &dbSet{drv: shard.Track(pg, t.TempDir())},
		iso: &isoRun{pg: pg, measure: true, begin: map[string]shard.Observation{}, spans: map[shard.TestID]shard.Span{},
			alone: map[string][]string{}, shared: "localci_iso_s", nShared: 2,
			known: map[shard.TestID]shard.Access{"t1": {Writes: []string{"a"}}, "t2": {Writes: []string{"b"}}}}}
	g.r.mu.Lock()
	g.r.live["9"] = ex
	g.r.mu.Unlock()
	hook := func(kind, test, db string, shardN int) {
		tc := TestCtx{Run: "9", Suite: "e2e", Shard: shardN, Test: test, DB: db}
		if kind == "begin" {
			g.r.isoBegin(tc)
		} else {
			g.r.isoEnd(tc)
		}
	}
	write := func(db, sql string) {
		if err := pg.Exec(ctx, db, sql); err != nil {
			t.Fatal(err)
		}
	}

	hook("begin", "t1", "localci_iso", 1)
	write("localci_iso", "insert into a values (1); select * from b")
	hook("end", "t1", "localci_iso", 1)
	ex.testOutcome("t1", "passed") // the report's test.end
	got, _ := g.st.Writes(ctx, "e2e")
	if w := got["t1"]; !slices.Equal(w.Tables, []string{"a"}) || !slices.Contains(w.Reads, "b") {
		t.Fatalf("t1 learned %+v, want writes [a] and a read of b", w)
	}

	if _, err := pg.Observe(ctx, "localci_iso_s", shard.ObserveOpts{Install: true, Since: shard.NoRows}); err != nil { // startShared's job
		t.Fatal(err)
	}
	hook("begin", "t1", "localci_iso_s", 1)
	hook("begin", "t2", "localci_iso_s", 2)
	write("localci_iso_s", "insert into a values (1)") // known to t1: explained
	write("localci_iso_s", "insert into x values (1)") // known to nobody
	hook("end", "t1", "localci_iso_s", 1)
	hook("end", "t2", "localci_iso_s", 2)
	ex.closeShared()
	if len(ex.iso.suspects) != 2 || !slices.Equal(ex.iso.suspects[0].Unexplained, []string{"x"}) {
		t.Fatalf("suspects = %+v, want t1 and t2 flagged for x", ex.iso.suspects)
	}
	if ex.iso.shared != "" {
		t.Fatal("the shared database must be dropped after the parallel part")
	}
	if pg.Exec(ctx, "localci_iso_s", "select 1") == nil {
		t.Fatal("the shared database is still there")
	}
}

// What the tenant flow writes is the core's setup: it falls inside the first tests' spans, and flags none of them.
// The same write outside the flow does, so the window is not hiding everything.
func TestWritesOfTheTenantFlowFlagNoSuspects(t *testing.T) {
	pg := startTestPostgres(t)
	ctx := context.Background()
	mustExec := func(db, sql string) {
		t.Helper()
		if err := pg.Exec(ctx, db, sql); err != nil {
			t.Fatal(err)
		}
	}
	g := newRig(t, fakeSuite("    isolation: auto\n"), []string{"pass1"}, 4)
	for _, c := range []struct {
		name    string
		inFlow  bool
		suspect bool
	}{{"inside the tenant flow", true, false}, {"outside it", false, true}} {
		db := "localci_tn_" + strings.ReplaceAll(c.name[:2], " ", "")
		mustExec("postgres", "create database "+db)
		mustExec(db, "create table a(v int); create table tenants(v int)")
		ex := &execution{r: g.r, suite: g.r.suites["e2e"], id: "11", ctx: ctx, bg: ctx,
			db: &dbSet{drv: shard.Track(pg, t.TempDir())},
			iso: &isoRun{pg: pg, measure: true, begin: map[string]shard.Observation{}, spans: map[shard.TestID]shard.Span{},
				alone: map[string][]string{}, open: map[string]int64{}, quiet: map[string]shard.Span{}, shared: db, nShared: 2, tenant: "make-tenant",
				known: map[shard.TestID]shard.Access{"t1": {Writes: []string{"a"}}, "t2": {Writes: []string{"a"}}}}}
		g.r.mu.Lock()
		g.r.live["11"] = ex
		g.r.mu.Unlock()
		if _, err := pg.Observe(ctx, db, shard.ObserveOpts{Install: true, Since: shard.NoRows}); err != nil {
			t.Fatal(err)
		}
		in := &ingest{ex: ex, n: 1}
		for _, id := range []string{"t1", "t2"} {
			g.r.isoBegin(TestCtx{Run: "11", Suite: "e2e", Shard: 1, Test: id, DB: db})
		}
		if c.inFlow {
			in.tenantFlowBegin(&report.FlowBegin{Flow: "make-tenant", Step: "t1:f1"})
		}
		mustExec(db, "insert into tenants values (1)") // known to neither test
		if c.inFlow {
			in.tenantFlowEnd(&report.FlowEnd{Step: "t1:f1"})
		}
		mustExec(db, "insert into a values (1)")
		for _, id := range []string{"t1", "t2"} {
			g.r.isoEnd(TestCtx{Run: "11", Suite: "e2e", Shard: 1, Test: id, DB: db})
		}
		ex.closeShared()
		if got := len(ex.iso.suspects) > 0; got != c.suspect {
			t.Errorf("write %s: suspects = %+v, want suspects: %v", c.name, ex.iso.suspects, c.suspect)
		}
	}
}

// An app database that has a schema of the log's name is refused with an error that names the setting.
func TestWritesLogSchemaOfTheAppIsRefusedByName(t *testing.T) {
	pg := startTestPostgres(t)
	ctx := context.Background()
	if err := pg.Exec(ctx, "postgres", "create database localci_app"); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(ctx, "localci_app", "create schema localci; create table localci.accounts(id int)"); err != nil {
		t.Fatal(err)
	}
	g := newRig(t, fakeSuite("    isolation: auto\n"), []string{"pass1"}, 4)
	ex := &execution{r: g.r, suite: g.r.suites["e2e"], id: "12", ctx: ctx, bg: ctx, iso: &isoRun{pg: pg, measure: true, schema: "localci"}}
	err := ex.checkLogSchema(ctx, "localci_app")
	if err == nil || !strings.Contains(err.Error(), "suites.e2e.writes.schema") {
		t.Fatalf("error = %v, want one that names suites.e2e.writes.schema", err)
	}
	ex.iso.schema = "wlog"
	if err := ex.checkLogSchema(ctx, "localci_app"); err != nil {
		t.Fatalf("with the setting changed: %v", err)
	}
}

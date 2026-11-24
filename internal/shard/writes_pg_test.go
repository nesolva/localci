package shard

import (
	"context"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

func mustExec(t *testing.T, pg *Postgres, db, sql string) {
	t.Helper()
	if err := pg.Exec(context.Background(), db, sql); err != nil {
		t.Fatal(err)
	}
}

// appConn is a connection of another role that stays open and idle between statements, as an app's pool does.
func appConn(t *testing.T, pg *Postgres, db string) (send func(string), closeConn func()) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", pg.Container, "psql", "-X", "-q", "-U", "app", "-d", db)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	closeConn = func() {
		if !closed {
			closed = true
			_ = in.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(closeConn)
	return func(s string) { _, _ = io.WriteString(in, s+"\n") }, closeConn
}

const fixture = `create role app login;
create table orders(id int primary key, v int);
create table audit(v int);
create table plain(v int);
grant all on all tables in schema public to app;`

// The write log sees what an app connection writes, however late Postgres's counters would show it: the write
// comes less than a second after the connection's previous one (the case where pg_stat_user_tables lags by about
// 10 s), over a role that cannot touch the log's schema.
func TestWriteLogSeesWritesOfAnIdleAppConnection(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	mustExec(t, pg, "postgres", "create database localci_wfix")
	mustExec(t, pg, "localci_wfix", fixture)

	begin, err := pg.Observe(ctx, "localci_wfix", ObserveOpts{Install: true, Since: NoRows, Counters: true})
	if err != nil {
		t.Fatal(err)
	}
	send, _ := appConn(t, pg, "localci_wfix")
	send("insert into audit values (1);")
	time.Sleep(300 * time.Millisecond)
	send("insert into orders values (1, 1);")
	send("update orders set v = 2 where id = 1;")
	send("select 1;")
	mustExec(t, pg, "localci_wfix", "select * from plain") // a session that ends reports its reads at once
	time.Sleep(300 * time.Millisecond)                     // the app's statements are asynchronous; no waiting for counters

	end, err := pg.Observe(ctx, "localci_wfix", ObserveOpts{Since: begin.Seq, Counters: true})
	if err != nil {
		t.Fatal(err)
	}
	got := Measure(begin, end)
	if !slices.Equal(got.Writes, []string{"audit", "orders"}) {
		t.Fatalf("writes = %v, want [audit orders]", got.Writes)
	}
	if !slices.Contains(got.Reads, "plain") || !slices.Contains(got.Reads, "orders") {
		t.Fatalf("reads = %v, want plain and the written tables", got.Reads)
	}
	for _, set := range [][]string{got.Writes, got.Reads} {
		for _, tbl := range set {
			if tbl == "localci.writes" || tbl == "writes" {
				t.Fatalf("the log leaked into a set: %v", set)
			}
		}
	}
}

// The log survives a database copy (checkpoints), installs again without harm, and covers a table made later.
func TestWriteLogSurvivesACopyAndInstallsAgain(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	mustExec(t, pg, "postgres", "create database localci_wsrc")
	mustExec(t, pg, "localci_wsrc", fixture)
	if _, err := pg.Observe(ctx, "localci_wsrc", ObserveOpts{Install: true, Since: NoRows}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pg, "localci_wsrc", "insert into audit values (1)")
	if err := pg.Clone(ctx, "localci_wsrc", "localci_wcopy"); err != nil {
		t.Fatal(err)
	}

	begin, err := pg.Observe(ctx, "localci_wcopy", ObserveOpts{Install: true, Since: NoRows}) // again: no error, one trigger
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pg, "localci_wcopy", "create table later(v int); insert into plain values (1)")
	mid, err := pg.Observe(ctx, "localci_wcopy", ObserveOpts{Install: true, Since: begin.Seq})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pg, "localci_wcopy", "insert into later values (1); insert into audit values (2)")
	end, err := pg.Observe(ctx, "localci_wcopy", ObserveOpts{Since: mid.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if got := Measure(mid, end).Writes; !slices.Equal(got, []string{"audit", "later"}) {
		t.Fatalf("writes after the copy = %v, want [audit later] (one row per statement, no doubles)", got)
	}
	if n := len(end.Writes); n != 2 {
		t.Fatalf("%d log rows for two statements, want 2: %v", n, end.Writes)
	}
}

// window is what a test measures: from where the previous one stopped to now.
func window(t *testing.T, pg *Postgres, db string, prev Observation, o ObserveOpts) (Access, Observation) {
	t.Helper()
	o.Since, o.XidFrom = prev.Seq, prev.Xmin
	next, err := pg.Observe(context.Background(), db, o)
	if err != nil {
		t.Fatal(err)
	}
	return Measure(prev, next), next
}

// The log's sequence number is not the order of commits: a transaction that took the lower number and commits
// after a look at the log is not behind that look. A window that reads "rows after the highest number seen" loses
// its write for good; a window that starts where the last one stopped, in transaction ids, finds it.
func TestWriteLogFindsAWriteThatCommitsAfterTheLookBehindIt(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	mustExec(t, pg, "postgres", "create database localci_wseq")
	mustExec(t, pg, "localci_wseq", fixture)
	begin, err := pg.Observe(ctx, "localci_wseq", ObserveOpts{Install: true, Since: NoRows})
	if err != nil {
		t.Fatal(err)
	}

	slow, commitSlow := appConn(t, pg, "localci_wseq")
	slow("begin; insert into audit values (1);") // takes the lower sequence number, stays open
	time.Sleep(300 * time.Millisecond)
	mustExec(t, pg, "localci_wseq", "insert into orders values (1, 1)") // higher number, commits at once
	first, mid := window(t, pg, "localci_wseq", begin, ObserveOpts{})
	if !slices.Contains(first.Writes, "orders") {
		t.Fatalf("first window = %v, want the committed write", first.Writes)
	}

	if slices.Contains(first.Writes, "audit") {
		t.Fatal("an uncommitted write cannot be in the window")
	}

	slow("commit;")
	commitSlow()
	second, _ := window(t, pg, "localci_wseq", mid, ObserveOpts{})
	if !slices.Contains(second.Writes, "audit") {
		t.Fatalf("second window = %v: the write that committed late was lost", second.Writes)
	}
}

// A table created while the log is on is logged from its creation, with or without a row: in the shared
// database (rows read whole) and in a measured window.
func TestWriteLogSeesTablesCreatedAfterInstall(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	mustExec(t, pg, "postgres", "create database localci_wddl")
	mustExec(t, pg, "localci_wddl", fixture)
	begin, err := pg.Observe(ctx, "localci_wddl", ObserveOpts{Install: true, Since: NoRows, Tables: true})
	if err != nil {
		t.Fatal(err)
	}
	if !begin.DDL {
		t.Fatal("the postgres of the tests is a superuser one: the event trigger must be there")
	}
	mustExec(t, pg, "localci_wddl", "create table made(v int); create table filled as select 1 as v; insert into filled values (2)")
	got, _ := window(t, pg, "localci_wddl", begin, ObserveOpts{Tables: true})
	if !slices.Equal(got.Writes, []string{"filled", "made"}) {
		t.Fatalf("measured window: writes = %v, want [filled made]", got.Writes)
	}
	all, err := pg.Observe(ctx, "localci_wddl", ObserveOpts{Since: 0}) // the shared database's whole log
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, w := range all.Writes {
		seen[w.Table] = true
	}
	if !seen["made"] || !seen["filled"] {
		t.Fatalf("the log has %v, want made and filled", all.Writes)
	}

	// Without the event trigger (no superuser to make it) the tables that appeared in the window count as written.
	mustExec(t, pg, "localci_wddl", "drop event trigger localci_ddl")
	begin2, err := pg.Observe(ctx, "localci_wddl", ObserveOpts{Since: NoRows, Tables: true})
	if err != nil {
		t.Fatal(err)
	}
	if begin2.DDL {
		t.Fatal("the event trigger is gone")
	}
	mustExec(t, pg, "localci_wddl", "create table without_trigger(v int)")
	got, _ = window(t, pg, "localci_wddl", begin2, ObserveOpts{Tables: true})
	if !slices.Equal(got.Writes, []string{"without_trigger"}) {
		t.Fatalf("fallback window: writes = %v, want [without_trigger]", got.Writes)
	}
}

// A database of the app that already has a schema of the log's name is refused, never written into; with another
// name for the log it works and the app's own tables are measured like any.
func TestWriteLogRefusesASchemaItDidNotMake(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	mustExec(t, pg, "postgres", "create database localci_wown")
	mustExec(t, pg, "localci_wown", fixture+"; create schema localci; create table localci.accounts(id int)")

	taken, err := pg.LogSchemaTaken(ctx, "localci_wown", DefaultLogSchema)
	if err != nil || !taken {
		t.Fatalf("LogSchemaTaken = %v, %v, want true", taken, err)
	}
	_, err = pg.Observe(ctx, "localci_wown", ObserveOpts{Install: true, Since: NoRows})
	if e, ok := IsSchemaTaken(err); !ok || e.Schema != "localci" {
		t.Fatalf("install error = %v, want a refusal naming the schema", err)
	}
	var n string
	if err := pg.Exec(ctx, "localci_wown", "select 1 from information_schema.tables where table_schema = 'localci' and table_name = 'writes'"); err != nil {
		t.Fatal(err)
	}
	if out, _ := pg.command(ctx, "psql", "localci_wown", "-At", "-c", "select count(*) from information_schema.tables where table_schema = 'localci'").CombinedOutput(); strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("the app's schema was touched: %s %s", out, n)
	}

	begin, err := pg.Observe(ctx, "localci_wown", ObserveOpts{Install: true, Schema: "wlog", Since: NoRows, Counters: true})
	if err != nil {
		t.Fatalf("with another schema: %v", err)
	}
	mustExec(t, pg, "localci_wown", "insert into localci.accounts values (1)")
	got, _ := window(t, pg, "localci_wown", begin, ObserveOpts{Schema: "wlog", Counters: true})
	if !slices.Equal(got.Writes, []string{"localci.accounts"}) {
		t.Fatalf("writes = %v, want the app's table", got.Writes)
	}
	if taken, _ := pg.LogSchemaTaken(ctx, "localci_wown", "wlog"); taken {
		t.Fatal("the schema the core made is its own")
	}
	if _, err := pg.Observe(ctx, "localci_wown", ObserveOpts{Install: true, Schema: "wlog", Since: NoRows}); err != nil {
		t.Fatalf("installing again: %v", err)
	}
}

func TestCheckLogSchema(t *testing.T) {
	for _, ok := range []string{"localci", "wlog", "_x1"} {
		if err := CheckLogSchema(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Log", "a-b", "pg_x", "public", "information_schema", "1a", "a;drop"} {
		if CheckLogSchema(bad) == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

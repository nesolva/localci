package shard

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startPostgres runs a throwaway PostgreSQL in Docker; the test is skipped without Docker.
func startPostgres(t *testing.T) *Postgres {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	name := fmt.Sprintf("localci-test-pg-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-e", "POSTGRES_PASSWORD=test", "postgres:17").CombinedOutput(); err != nil {
		t.Skipf("cannot start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	pg := &Postgres{Container: name}
	deadline := time.Now().Add(60 * time.Second)
	for {
		// The image starts a temporary server to initialise the cluster, stops it and starts the real one: ready
		// means "ready to accept connections" was logged twice.
		logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Count(string(logs), "ready to accept connections") >= 2 {
			if _, err := pg.exists(context.Background(), "postgres"); err == nil {
				return pg
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("postgres did not become ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (p *Postgres) scalar(t *testing.T, db, sql string) string {
	t.Helper()
	out, err := p.command(context.Background(), "psql", db, "-At", "-c", sql).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v: %s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPostgresSnapshotAndRestoreRoundTripWithAClientThatReconnects(t *testing.T) {
	pg := startPostgres(t)
	ctx := context.Background()
	live := ShardName(testInstance, "acme", "r1", 1)
	if err := pg.Exec(ctx, "postgres", "create database "+live); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(ctx, live, "create table sessions(id int); insert into sessions values (1)"); err != nil {
		t.Fatal(err)
	}

	// The application holds a connection to the shard database while the snapshot is taken.
	app := pg.command(ctx, "psql", live, "-At", "-c", "select pg_sleep(300)")
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- app.Wait() }()
	for i := 0; pg.scalar(t, "postgres", "select count(*) from pg_stat_activity where datname = '"+live+"' and query like '%pg_sleep%' and pid <> pg_backend_pid()") != "1"; i++ {
		if i > 40 {
			t.Fatal("the client never connected")
		}
		time.Sleep(250 * time.Millisecond)
	}

	ck := "localci_ck_roundtrip0001"
	if err := pg.Snapshot(ctx, live, ck); err != nil {
		t.Fatalf("snapshot with a connected client: %v", err)
	}
	if ok, err := pg.Complete(ctx, ck); err != nil || !ok {
		t.Fatalf("the snapshot is not marked complete: %v %v", ok, err)
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the connected client was not disconnected for the copy")
	}
	if size, err := pg.Size(ctx, ck); err != nil || size <= 0 {
		t.Errorf("Size = %d, %v", size, err)
	}

	// The client reconnects: the live database accepts connections again and keeps working.
	if err := pg.Exec(ctx, live, "insert into sessions values (2)"); err != nil {
		t.Fatalf("the application could not reconnect after the snapshot: %v", err)
	}
	// Restore: drop the live database and clone the snapshot into its place.
	if err := pg.Drop(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := pg.Clone(ctx, ck, live); err != nil {
		t.Fatal(err)
	}
	if got := pg.scalar(t, live, "select count(*) from sessions"); got != "1" {
		t.Errorf("after restore the shard has %s rows, want the snapshot's 1", got)
	}
	if err := pg.Exec(ctx, live, "insert into sessions values (3)"); err != nil {
		t.Errorf("the restored database is not usable: %v", err)
	}
	if got := pg.scalar(t, ck, "select count(*) from sessions"); got != "1" {
		t.Errorf("the checkpoint itself changed: %s rows", got)
	}
}

func TestPostgresSnapshotKilledMidCopyLeavesNothingRestorable(t *testing.T) {
	pg := startPostgres(t)
	ctx := context.Background()
	live := ShardName(testInstance, "acme", "r2", 1)
	if err := pg.Exec(ctx, "postgres", "create database "+live); err != nil {
		t.Fatal(err)
	}
	if err := pg.Exec(ctx, live, "create table blob as select repeat('x', 1000) as v from generate_series(1, 150000)"); err != nil {
		t.Fatal(err)
	}
	ck := "localci_ck_killed00000001"

	// A copy that stopped half way, under the final name, is not complete and is replaced by the next snapshot.
	if err := pg.Exec(ctx, "postgres", "create database "+ck); err != nil {
		t.Fatal(err)
	}
	if ok, _ := pg.Complete(ctx, ck); ok {
		t.Fatal("a database without the completion mark reads as complete")
	}

	// A snapshot killed while copying never leaves a complete one, and no temporary database behind.
	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	err := pg.Snapshot(short, live, ck)
	if err == nil {
		t.Log("the copy finished before the deadline: nothing was killed on this machine")
	} else if ok, _ := pg.Complete(ctx, ck); ok {
		t.Errorf("a snapshot that failed (%v) is marked complete", err)
	}
	time.Sleep(time.Second) // the copy's backend stops once its client is gone
	if left := pg.scalar(t, "postgres", "select count(*) from pg_database where datname like '"+ck+"\\_b%'"); left != "0" {
		t.Errorf("%s temporary databases left behind", left)
	}

	// The next snapshot completes whatever was left.
	if err := pg.Snapshot(ctx, live, ck); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.Complete(ctx, ck); err != nil || !ok {
		t.Errorf("complete = %v, %v", ok, err)
	}
	if got := pg.scalar(t, ck, "select count(*) from blob"); got != "150000" {
		t.Errorf("the finished snapshot has %s rows", got)
	}
}

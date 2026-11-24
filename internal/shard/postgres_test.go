package shard

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Runs against a real PostgreSQL in a throwaway container; skipped without Docker.
func TestPostgresTemplateCloneDrop(t *testing.T) {
	pg := startPostgres(t)
	ctx := context.Background()

	builds := 0
	seed := func(ctx context.Context, db string) error {
		builds++ // about 50 MB of rows
		return pg.Exec(ctx, db, "create table blob as select repeat('x', 1000) as v from generate_series(1, 50000)")
	}
	tpl, err := pg.Template(ctx, "k1", seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Template(ctx, "k1", seed); err != nil || builds != 1 {
		t.Fatalf("template must be built once per key: builds=%d err=%v", builds, err)
	}

	shard := ShardName(testInstance, "acme", "r1", 0)
	start := time.Now()
	if err := pg.Clone(ctx, tpl, shard); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("cloning took %v, want under 1s", took)
	}
	if err := pg.Exec(ctx, shard, "do $$ begin if (select count(*) from blob) <> 50000 then raise exception 'clone is missing rows'; end if; end $$"); err != nil {
		t.Fatal(err)
	}
	if err := pg.Drop(ctx, shard); err != nil {
		t.Fatal(err)
	}
	if ok, err := pg.exists(ctx, shard); err != nil || ok {
		t.Fatalf("database still there after drop: ok=%v err=%v", ok, err)
	}
	if err := pg.Drop(ctx, "localci_"+strings.Repeat("a", 5)); err != nil {
		t.Fatalf("dropping a missing database must not fail: %v", err)
	}
}

func TestPostgresPasswordIsNotInArgv(t *testing.T) {
	pg := &Postgres{URL: "postgresql://app:s3cret@db.example:5432/postgres?sslmode=require"}
	for _, cmd := range []*exec.Cmd{
		pg.tool(context.Background(), "psql", "-c", "select 1"),
		pg.tool(context.Background(), "createdb", "x"),
		pg.command(context.Background(), "psql", "mydb", "-c", "select 1"),
	} {
		if strings.Contains(strings.Join(cmd.Args, " "), "s3cret") {
			t.Fatalf("password on the command line: %v", cmd.Args)
		}
		if !slices.Contains(cmd.Env, "PGPASSWORD=s3cret") {
			t.Fatalf("password must travel in the environment: %v", cmd.Args)
		}
	}
	t.Setenv("PGPASSWORD", "fromenv")
	d := (&Postgres{Container: "c1"}).tool(context.Background(), "psql")
	if strings.Contains(strings.Join(d.Args, " "), "fromenv") || !slices.Contains(d.Args, "PGPASSWORD") {
		t.Fatalf("docker exec must forward PGPASSWORD by name only: %v", d.Args)
	}
}

func startPG(t *testing.T) *Postgres {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	name := fmt.Sprintf("localci-test-pg-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=test", "postgres:17").CombinedOutput(); err != nil {
		t.Skipf("cannot start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	pg := &Postgres{Container: name}
	deadline := time.Now().Add(60 * time.Second)
	for {
		// The image first runs a temporary server on the socket only; the final one listens on TCP.
		if exec.Command("docker", "exec", name, "pg_isready", "-h", "127.0.0.1").Run() == nil {
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

func TestPostgresHalfBuiltTemplateIsNeverReused(t *testing.T) {
	pg := startPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := func(c context.Context, db string) error {
		_ = pg.Exec(c, db, "create table half(a int)")
		cancel()
		return context.Canceled
	}
	if _, err := pg.Template(ctx, "k1", cancelling); err == nil {
		t.Fatal("expected the cancel error")
	}
	bg := context.Background()
	if out, _ := pg.run(bg, "psql", "-At", "-c", "select count(*) from pg_database where datname like 'localci_tpl_%'"); out != "0" {
		t.Fatalf("a cancelled seed left %s template database(s)", out)
	}
	// A leftover under the final name without the finished mark (hard kill, older build).
	if _, err := pg.run(bg, "createdb", "localci_tpl_k2"); err != nil {
		t.Fatal(err)
	}
	built := false
	seed := func(c context.Context, db string) error {
		built = true
		return pg.Exec(c, db, "create table done(a int)")
	}
	tpl, err := pg.Template(bg, "k2", seed)
	if err != nil || !built {
		t.Fatalf("an unfinished template must be rebuilt: built=%v err=%v", built, err)
	}
	if err := pg.Exec(bg, tpl, "select * from done"); err != nil {
		t.Fatalf("template is missing the seeded schema: %v", err)
	}
}

func TestPostgresFileCopyDoesNotCacheContextErrors(t *testing.T) {
	pg := startPG(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	pg.fileCopy(cancelled)
	if !pg.fileCopy(context.Background()) {
		t.Fatal("postgres 17 supports file_copy: a cancelled first call must not decide it")
	}
}

// A seed tool whose pool keeps a backend open on the temporary database for a
// moment after it returns must not make the final rename fail.
func TestPostgresTemplateSurvivesLingeringSeedSession(t *testing.T) {
	pg := startPG(t)
	bg := context.Background()
	var linger *exec.Cmd
	seed := func(c context.Context, db string) error {
		if err := pg.Exec(c, db, "create table seeded(a int)"); err != nil {
			return err
		}
		linger = pg.command(bg, "psql", db, "-c", "select pg_sleep(30)")
		if err := linger.Start(); err != nil {
			return err
		}
		for i := 0; i < 50; i++ {
			out, _ := pg.run(bg, "psql", "-At", "-c", fmt.Sprintf(
				"select count(*) from pg_stat_activity where datname = '%s'", db))
			if out != "0" {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("lingering session never connected")
	}
	tpl, err := pg.Template(bg, "linger", seed)
	if linger != nil && linger.Process != nil {
		_ = linger.Process.Kill()
		_ = linger.Wait()
	}
	if err != nil {
		t.Fatalf("a session left on the seeded database must not fail the template: %v", err)
	}
	if err := pg.Exec(bg, tpl, "select * from seeded"); err != nil {
		t.Fatalf("template is missing the seeded schema: %v", err)
	}
}

package shard

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperProcess re-runs this test binary as one of the processes of a test below; its work is picked by
// $LOCALCI_TEST_PROC and done in the test it names, which is a no-op in a normal run.
func helperProcess(t *testing.T, test string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// Process of TestTwoProcessesBuildATemplateOnce: builds the template in $LOCALCI_TEST_STATE through the command
// driver; its seed adds a line to the build log, so the parent can count the builds.
func TestTemplateBuilderProcess(t *testing.T) {
	state := os.Getenv("LOCALCI_TEST_STATE")
	if state == "" {
		t.Skip("helper process")
	}
	c := &Command{StateDir: filepath.Join(state, "templates"), DropCmd: "true", CreateCmd: "true"}
	seed := func(context.Context, string) error {
		f, err := os.OpenFile(filepath.Join(state, "builds"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		time.Sleep(600 * time.Millisecond) // long enough for the other process to arrive and wait
		_, err = fmt.Fprintln(f, os.Getpid())
		return err
	}
	if _, err := c.Template(context.Background(), "k1", seed); err != nil {
		t.Fatal(err)
	}
}

func TestTwoProcessesBuildATemplateOnce(t *testing.T) {
	state := t.TempDir()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := helperProcess(t, "TestTemplateBuilderProcess", "LOCALCI_TEST_STATE="+state).CombinedOutput(); err != nil {
				t.Errorf("builder: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(state, "builds"))
	if n := len(strings.Fields(string(b))); n != 1 {
		t.Fatalf("the template was built %d times by two processes, want once (the other waits and reuses it)", n)
	}
}

// Process of TestSweepAfterAKillDuringATemplateBuild: starts a template build on the server of $LOCALCI_TEST_PG
// that never ends, and says when its temporary database exists.
func TestSlowTemplateBuildProcess(t *testing.T) {
	container := os.Getenv("LOCALCI_TEST_PG")
	if container == "" {
		t.Skip("helper process")
	}
	pg := &Postgres{Container: container, LockDir: os.Getenv("LOCALCI_TEST_LOCKS")}
	seed := func(ctx context.Context, name string) error {
		if err := pg.Exec(ctx, name, "create table half(a int)"); err != nil {
			return err
		}
		if err := os.WriteFile(os.Getenv("LOCALCI_TEST_READY"), nil, 0o644); err != nil {
			return err
		}
		time.Sleep(time.Hour) // the parent kills this process
		return nil
	}
	_, _ = pg.Template(context.Background(), "k9", seed)
}

func TestSweepAfterAKillDuringATemplateBuild(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	pg.LockDir = t.TempDir()
	list := func() []string {
		names, err := pg.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	mustRun := func(args ...string) {
		t.Helper()
		if _, err := pg.run(ctx, args[0], args[1:]...); err != nil {
			t.Fatal(err)
		}
	}

	// What must survive: a finished checkpoint, and the databases of a run still running or unknown here.
	mustRun("createdb", "localci_src")
	if err := pg.Snapshot(ctx, "localci_src", "localci_ck_marked"); err != nil {
		t.Fatal(err)
	}
	// A suite named ck-web has shards that start like a checkpoint.
	running, unknown := ShardName(testInstance, "ck-web", "running", 0), ShardName(testInstance, "web", "elsewhere", 0)
	mustRun("createdb", running)
	mustRun("createdb", unknown)
	// What a finished run leaves, and what a kill during a checkpoint save leaves (a copy under its temporary name,
	// and one under its final name without the mark).
	ended, endedShared := ShardName(testInstance, "web", "ended", 1), SharedName(testInstance, "web", "ended")
	mustRun("createdb", ended)
	mustRun("createdb", endedShared)
	mustRun("createdb", "localci_ck_half_b1700000000000000000")
	mustRun("createdb", "localci_ck_half2")
	runs := []RunRef{{"ck-web", "running", false}, {"web", "ended", true}}

	// A template build that is alive is left alone, whatever its database looks like.
	ready := filepath.Join(t.TempDir(), "ready")
	build := helperProcess(t, "TestSlowTemplateBuildProcess",
		"LOCALCI_TEST_PG="+pg.Container, "LOCALCI_TEST_LOCKS="+pg.LockDir, "LOCALCI_TEST_READY="+ready)
	if err := build.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = build.Process.Kill(); _ = build.Wait() })
	waitFor(t, func() bool { _, err := os.Stat(ready); return err == nil })
	tmpTemplate := func() []string {
		return slices.DeleteFunc(list(), func(n string) bool { return !strings.HasPrefix(n, "localci_tpl_k9_b") })
	}
	if len(tmpTemplate()) != 1 {
		t.Fatalf("the build has no temporary database: %v", list())
	}
	dropped, err := SweepOrphans(ctx, pg, testInstance, runs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"localci_ck_half2", "localci_ck_half_b1700000000000000000", ended, endedShared}
	if !slices.Equal(sorted(dropped), sorted(want)) {
		t.Fatalf("dropped %v, want %v (and not the template a live process is building)", dropped, want)
	}
	if len(tmpTemplate()) != 1 {
		t.Fatal("the sweep dropped the template a live process is building")
	}

	// kill -9 in the middle of the build, then the next start.
	if err := build.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = build.Wait()
	// a leftover is only taken for dead once it is older than any build can last
	sweepNow = func() time.Time { return time.Now().Add(2 * buildMaxAge) }
	t.Cleanup(func() { sweepNow = time.Now })
	dropped, err = SweepOrphans(ctx, pg, testInstance, runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || !strings.HasPrefix(dropped[0], "localci_tpl_k9_b") || len(tmpTemplate()) != 0 {
		t.Fatalf("after the kill dropped %v, want the half-built template only", dropped)
	}
	left := list()
	for _, keep := range []string{"localci_ck_marked", running, unknown, "localci_src"} {
		if !slices.Contains(left, keep) {
			t.Errorf("the sweep dropped %s", keep)
		}
	}
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// Two localci instances (two data dirs) can share one Postgres server, and their run ids are sequential, so both
// have a "web" run "r1". One instance's sweep must not drop the live databases of the other's run of that name, nor a
// template build that may belong to the other.
func TestSweepLeavesWhatAnotherInstanceMadeOnTheSameServer(t *testing.T) {
	pg := startPG(t)
	ctx := context.Background()
	pg.LockDir = t.TempDir() // this instance's locks: the other instance's builds are invisible here
	mustRun := func(name string) {
		t.Helper()
		if _, err := pg.run(ctx, "createdb", name); err != nil {
			t.Fatal(err)
		}
	}
	list := func() []string {
		names, err := pg.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	theirs, theirsShared := ShardName("inst-b", "web", "r1", 1), SharedName("inst-b", "web", "r1")
	mine := ShardName("inst-a", "web", "r1", 1)
	freshBuild := fmt.Sprintf("localci_tpl_k7_b%d", time.Now().UnixNano())
	oldBuild := fmt.Sprintf("localci_tpl_k8_b%d", time.Now().Add(-2*buildMaxAge).UnixNano())
	for _, n := range []string{theirs, theirsShared, mine, freshBuild, oldBuild} {
		mustRun(n)
	}

	// inst-a's store knows its own run r1 as finished.
	dropped, err := SweepOrphans(ctx, pg, "inst-a", []RunRef{{"web", "r1", true}})
	if err != nil {
		t.Fatal(err)
	}
	if want := sorted([]string{mine, oldBuild}); !slices.Equal(sorted(dropped), want) {
		t.Fatalf("dropped %v, want %v: its own finished run and the build that is too old to be alive", sorted(dropped), want)
	}
	left := list()
	for _, keep := range []string{theirs, theirsShared, freshBuild} {
		if !slices.Contains(left, keep) {
			t.Errorf("the sweep dropped %s, which another instance may be using", keep)
		}
	}
}

package shard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFileDB(t *testing.T) (*Command, string) {
	t.Helper()
	dir := t.TempDir()
	return &Command{
		CreateCmd: "cp " + dir + "/{{template}} " + dir + "/{{name}}",
		DropCmd:   "rm -f " + dir + "/{{name}}",
		StateDir:  filepath.Join(t.TempDir(), "state"),
	}, dir
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func TestCommandCloneAndDrop(t *testing.T) {
	db, dir := newFileDB(t)
	ctx := context.Background()
	builds := 0
	seed := func(_ context.Context, name string) error {
		builds++
		return os.WriteFile(filepath.Join(dir, name), []byte("seeded"), 0o644)
	}
	tpl, err := db.Template(ctx, "abc123", seed)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := db.Template(ctx, "abc123", seed); err != nil || again != tpl || builds != 1 {
		t.Fatalf("template must be built once per key: builds=%d err=%v", builds, err)
	}

	name := ShardName(testInstance, "acme", "r1", 0)
	if err := db.Clone(ctx, tpl, name); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, name)); string(b) != "seeded" {
		t.Fatalf("clone content = %q", b)
	}
	if err := db.Drop(ctx, name); err != nil || exists(filepath.Join(dir, name)) {
		t.Fatalf("drop failed: %v", err)
	}
	if err := db.Drop(ctx, name); err != nil {
		t.Fatalf("dropping a missing database must not fail: %v", err)
	}
}

func TestCommandFailedSeedLeavesNoTemplate(t *testing.T) {
	db, dir := newFileDB(t)
	ctx := context.Background()
	failing := func(_ context.Context, name string) error {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("half"), 0o644)
		return os.ErrInvalid
	}
	if _, err := db.Template(ctx, "k1", failing); err == nil {
		t.Fatal("expected the seed error")
	}
	if exists(filepath.Join(dir, "localci_tpl_k1")) {
		t.Fatal("half-built template left behind")
	}
	built := false
	ok := func(_ context.Context, name string) error {
		built = true
		return os.WriteFile(filepath.Join(dir, name), nil, 0o644)
	}
	if _, err := db.Template(ctx, "k1", ok); err != nil || !built {
		t.Fatalf("a failed build must be retried: built=%v err=%v", built, err)
	}
}

func TestCommandRejectsNamesThatReachTheShell(t *testing.T) {
	db, _ := newFileDB(t)
	if err := db.Drop(context.Background(), "x; touch pwned"); err == nil {
		t.Fatal("a name with shell characters must be refused")
	}
}

func TestSweepDropsOnlyDatabasesOlderThan24h(t *testing.T) {
	db, dir := newFileDB(t)
	ctx := context.Background()
	tr := Track(db, filepath.Join(t.TempDir(), "ledger"))
	if err := os.WriteFile(filepath.Join(dir, "tpl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, fresh := ShardName(testInstance, "acme", "old", 0), ShardName(testInstance, "acme", "new", 0)
	for _, n := range []string{old, fresh} {
		if err := tr.Clone(ctx, "tpl", n); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(filepath.Join(tr.dir, old), past, past); err != nil {
		t.Fatal(err)
	}

	dropped, err := tr.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != old {
		t.Fatalf("dropped = %v, want [%s]", dropped, old)
	}
	if exists(filepath.Join(dir, old)) || !exists(filepath.Join(dir, fresh)) {
		t.Fatal("the old database must be gone and the recent one kept")
	}
	if err := tr.Drop(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if again, _ := tr.Sweep(ctx); len(again) != 0 {
		t.Fatalf("nothing should be left to sweep, got %v", again)
	}
}

func TestShardNameFitsIdentifierLimit(t *testing.T) {
	n := ShardName(testInstance, "A Very-Long suite name that goes on and on and on forever", "2026-11-19T10:00", 12)
	if len(n) > 63 {
		t.Fatalf("%q is %d long", n, len(n))
	}
	if got := ShardName(testInstance, "e2e", "run7", 3); got != "localci_e2e_run7_"+hashOf("e2e", "run7")+"_3" {
		t.Fatalf("got %q", got)
	}
}

func TestKeyChangesWithContent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "schema.sql")
	_ = os.WriteFile(f, []byte("create table a();"), 0o644)
	k1, err := Key([]string{f})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(f, []byte("create table b();"), 0o644)
	if k2, _ := Key([]string{f}); k2 == k1 {
		t.Fatal("a changed file must change the key")
	}
}

const testInstance = "inst-a"

func hashOf(suite, run string) string {
	n := ShardName(testInstance, suite, run, 0)
	parts := strings.Split(n, "_")
	return parts[len(parts)-2]
}

func TestShardNameSeparatesSuitesThatCleanToTheSameText(t *testing.T) {
	long := strings.Repeat("checkout-flow-", 6)
	pairs := [][2]string{
		{ShardName(testInstance, "web-e2e", "r1", 0), ShardName(testInstance, "web.e2e", "r1", 0)},
		{ShardName(testInstance, long+"a", "r1", 0), ShardName(testInstance, long+"b", "r1", 0)},
		{ShardName(testInstance, "web", "run-1", 0), ShardName(testInstance, "web", "run.1", 0)},
	}
	for _, p := range pairs {
		if p[0] == p[1] {
			t.Fatalf("collision: %s", p[0])
		}
		if len(p[0]) > 63 || len(p[1]) > 63 {
			t.Fatalf("too long: %s %s", p[0], p[1])
		}
	}
	if len(ShardName(testInstance, "s", strings.Repeat("r", 200), 123)) > 63 {
		t.Fatal("a long run id must still fit")
	}
}

func TestDropRefusesDatabasesThatAreNotLocalci(t *testing.T) {
	db, dir := newFileDB(t)
	if err := os.WriteFile(filepath.Join(dir, "postgres"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.Drop(context.Background(), "postgres"); err == nil || !exists(filepath.Join(dir, "postgres")) {
		t.Fatalf("a database without the localci prefix must not be dropped (err=%v)", err)
	}
	pg := &Postgres{}
	if err := pg.Drop(context.Background(), "postgres"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("postgres driver must refuse too: %v", err)
	}
}

func TestCommandSeedCancelledStillCleansAndRetryStartsClean(t *testing.T) {
	db, dir := newFileDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancelling := func(_ context.Context, name string) error {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("half"), 0o644)
		cancel()
		return context.Canceled
	}
	if _, err := db.Template(ctx, "k9", cancelling); err == nil {
		t.Fatal("expected the cancel error")
	}
	if exists(filepath.Join(dir, "localci_tpl_k9")) {
		t.Fatal("cleanup must run even when the context is cancelled")
	}
	// A hard kill leaves the half-built database and no marker: the retry must not seed into it.
	if err := os.WriteFile(filepath.Join(dir, "localci_tpl_k9"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := func(_ context.Context, name string) error {
		if exists(filepath.Join(dir, name)) {
			return os.ErrExist
		}
		return os.WriteFile(filepath.Join(dir, name), []byte("ok"), 0o644)
	}
	if _, err := db.Template(context.Background(), "k9", fresh); err != nil {
		t.Fatalf("retry seeded into a leftover: %v", err)
	}
}

type recordingDB struct {
	DB
	ledger string
	seen   bool
}

func (r *recordingDB) Clone(_ context.Context, _, name string) error {
	r.seen = exists(filepath.Join(r.ledger, name))
	return nil
}

func TestTrackerRecordsBeforeCloning(t *testing.T) {
	r := &recordingDB{ledger: filepath.Join(t.TempDir(), "ledger")}
	tr := Track(r, r.ledger)
	if err := tr.Clone(context.Background(), "t", "localci_x"); err != nil || !r.seen {
		t.Fatalf("the clone must be recorded before it is made: seen=%v err=%v", r.seen, err)
	}
}

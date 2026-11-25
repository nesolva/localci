package shard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nesolva/localci/internal/flock"
)

// Seed builds a template database that already exists and is empty: it runs
// the suite's migrations and fixtures against the database called name.
type Seed func(ctx context.Context, name string) error

// DB makes and removes the databases of a run. A shard gets a clone of the
// template, so it starts from migrated, seeded data in about a second.
type DB interface {
	// Template returns the name of the template database for key, building it
	// with seed when it does not exist yet. Concurrent calls build it once.
	Template(ctx context.Context, key string, seed Seed) (string, error)
	// Clone creates database name as a copy of template.
	Clone(ctx context.Context, template, name string) error
	// Drop removes database name; a missing database is not an error.
	Drop(ctx context.Context, name string) error
	// Snapshot copies the live database from to a new database name, for a
	// flow checkpoint. Postgres pauses the connections to from itself; the
	// command driver runs the suite's snapshot command, which must cope with
	// a live database.
	Snapshot(ctx context.Context, from, name string) error
}

// Completer is implemented by drivers whose snapshots carry a completion mark
// of their own (Postgres): a database that exists without it is the rest of
// a copy that never finished and must not be restored. Drivers without it
// rely on the checkpoint store, which records a checkpoint only once the
// snapshot returned.
type Completer interface {
	Complete(ctx context.Context, name string) (bool, error)
}

// Sizer is implemented by drivers that can say how big a database is, which
// the checkpoint store needs to keep its cache under the configured size.
type Sizer interface {
	Size(ctx context.Context, name string) (int64, error)
}

// cleanupTimeout bounds the cleanup that runs after a cancelled context.
const cleanupTimeout = 30 * time.Second

// StaleAfter is the age after which a shard database is a leftover of a run
// that never cleaned up.
const StaleAfter = 24 * time.Hour

var identRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

func checkIdent(name string) error {
	if !identRE.MatchString(name) {
		return fmt.Errorf("invalid database name %q: use letters, digits and underscores", name)
	}
	return nil
}

// Key is the template key: a hash of the files the suite lists (their paths
// and contents), so a changed migration or fixture builds a new template.
func Key(files []string) (string, error) {
	sorted := slices.Clone(files)
	slices.Sort(sorted)
	h := sha256.New()
	for _, f := range sorted {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("template key: %w", err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

var nameRE = regexp.MustCompile(`[^a-z0-9]+`)

// maxName is PostgreSQL's identifier limit; other engines are stricter, never looser.
const maxName = 63

// dbPrefix starts the name of every database localci makes (shards and
// templates). Drop refuses any other name, so a name that collided upstream
// can never remove a real database.
const dbPrefix = "localci_"

func checkDroppable(name string) error {
	if err := checkIdent(name); err != nil {
		return err
	}
	if !strings.HasPrefix(name, dbPrefix) {
		return fmt.Errorf("refusing to drop %q: not a localci database (names start with %s)", name, dbPrefix)
	}
	return nil
}

// ShardName is the database of shard n in a run:
// localci_<suite>_<run>_<hash>_<n>. Suite and run are lowercased and reduced
// to letters and digits, and long ones are cut to fit the identifier limit;
// the hash is taken from the instance id (see InstanceID) and the original
// suite and run, so names that clean or cut to the same text (web-e2e and
// web.e2e) still get different databases, and so do two localci instances
// whose run ids coincide on one server.
func ShardName(instance, suite, run string, n int) string {
	sum := sha256.Sum256([]byte(instance + "\x00" + suite + "\x00" + run))
	clean := func(s string) string { return strings.Trim(nameRE.ReplaceAllString(strings.ToLower(s), "_"), "_") }
	cleanSuite, cleanRun := clean(suite), clean(run)
	if len(cleanRun) > 24 {
		cleanRun = strings.TrimRight(cleanRun[:24], "_")
	}
	tail := fmt.Sprintf("_%s_%s_%d", cleanRun, hex.EncodeToString(sum[:4]), n)
	if room := maxName - len(dbPrefix) - len(tail); len(cleanSuite) > room {
		cleanSuite = cleanSuite[:max(room, 0)]
	}
	return dbPrefix + cleanSuite + tail
}

// Tracker wraps a DB and records every clone in a directory (one file per
// database, its modification time is the creation time), so a run that was
// killed before cleanup leaves a trail Sweep can follow whatever the engine.
type Tracker struct {
	DB
	dir string
	now func() time.Time
}

// Track records the clones made through db under dir.
func Track(db DB, dir string) *Tracker { return &Tracker{DB: db, dir: dir, now: time.Now} }

// Clone clones and records the new database.
func (t *Tracker) Clone(ctx context.Context, template, name string) error {
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return err
	}
	// Record first: a kill between the clone and the record would leak a
	// database nobody knows about. A record without a database is harmless,
	// Sweep drops with "if exists".
	if err := os.WriteFile(filepath.Join(t.dir, name), nil, 0o644); err != nil {
		return err
	}
	return t.DB.Clone(ctx, template, name)
}

// Drop drops and forgets the database. A failed drop stays recorded.
func (t *Tracker) Drop(ctx context.Context, name string) error {
	if err := t.DB.Drop(ctx, name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(t.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep drops the recorded databases older than StaleAfter and returns their
// names. Run it when a run starts. It keeps going after a failed drop and
// reports every failure.
func (t *Tracker) Sweep(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(t.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || e.IsDir() || t.now().Sub(info.ModTime()) < StaleAfter {
			continue
		}
		if err := t.Drop(ctx, e.Name()); err != nil {
			errs = append(errs, err)
			continue
		}
		dropped = append(dropped, e.Name())
	}
	return dropped, errors.Join(errs...)
}

// onceMap runs a build once per key, also across concurrent callers; a failed
// build is retried by the next call.
type onceMap struct {
	mu   sync.Mutex
	keys map[string]*sync.Mutex
}

func (o *onceMap) lock(key string) func() {
	o.mu.Lock()
	if o.keys == nil {
		o.keys = map[string]*sync.Mutex{}
	}
	m := o.keys[key]
	if m == nil {
		m = new(sync.Mutex)
		o.keys[key] = m
	}
	o.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lockPath is the file lock of the build called key in dir.
func lockPath(dir, key string) string { return filepath.Join(dir, key+".lock") }

// fileLock takes the cross-process lock; a variable so a test can make it fail like an NFS mount does.
var fileLock = flock.Lock

// lockIn is lock for builds that another localci process may run too: with a
// dir, a file lock there makes two processes take turns like two goroutines do,
// so the second finds the first one's result. Without a dir, or where the
// filesystem cannot lock files (NFS: ENOLCK, ENOTSUP; darwin also EOPNOTSUPP), only this process is
// serialised, as it was before the file lock existed. Any other lock error
// fails the caller, naming the lock file: carrying on unlocked would let two
// processes build the same thing at once.
func (o *onceMap) lockIn(ctx context.Context, dir, key string) (func(), error) {
	unlockMem := o.lock(key)
	if dir == "" {
		return unlockMem, nil
	}
	path := lockPath(dir, key)
	unlockFile, err := fileLock(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			unlockMem()
			return nil, ctx.Err()
		}
		if errors.Is(err, syscall.ENOLCK) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) {
			return unlockMem, nil
		}
		unlockMem()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { unlockFile(); unlockMem() }, nil
}

// IsComplete asks db whether the database name carries its completion mark. known is false when the driver has no
// such mark (the caller then relies on its own record).
func IsComplete(ctx context.Context, db DB, name string) (complete, known bool, err error) {
	if t, ok := db.(*Tracker); ok {
		db = t.DB
	}
	c, ok := db.(Completer)
	if !ok {
		return false, false, nil
	}
	complete, err = c.Complete(ctx, name)
	return complete, true, err
}

// SizeOf is the size of database name in bytes, 0 when the driver cannot tell.
func SizeOf(ctx context.Context, db DB, name string) int64 {
	if t, ok := db.(*Tracker); ok {
		db = t.DB
	}
	if s, ok := db.(Sizer); ok {
		if n, err := s.Size(ctx, name); err == nil {
			return n
		}
	}
	return 0
}

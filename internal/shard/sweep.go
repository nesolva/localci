package shard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/flock"
)

// LockDir is where the build locks of a data directory live, for the Postgres driver's LockDir.
func LockDir(dataDir string) string { return filepath.Join(dataDir, "locks") }

// RunRef is a run the state store knows, with whether it has ended.
type RunRef struct {
	Suite, ID string
	Finished  bool
}

// List returns the databases on the server that localci made (names starting with localci_).
func (p *Postgres) List(ctx context.Context) ([]string, error) {
	out, err := p.run(ctx, "psql", "-At", "-c",
		fmt.Sprintf("select datname from pg_database where left(datname, %d) = '%s' order by 1", len(dbPrefix), dbPrefix))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Fields(out) {
		if checkIdent(n) == nil {
			names = append(names, n)
		}
	}
	return names, nil
}

// shardShaped matches the end of a shard or shared database name (_<hash8>_<n> or _<hash8>_s). A suite called
// ck-web has shards that start like a checkpoint, and an unmarked one of a running run is no half-built checkpoint.
var shardShaped = regexp.MustCompile(`_[0-9a-f]{8}_([0-9]+|s)$`)

// tmpBuild matches the temporary name a template or checkpoint is built under: <final name>_b<nanos>.
var tmpBuild = regexp.MustCompile(`^(localci_(?:tpl|ck)_.+)_b[0-9]{10,}$`)

// buildMaxAge is how old a temporary build must be before it is taken for dead: the build lock of another localci
// is out of sight, so age is the only sign left. Far longer than any build.
const buildMaxAge = time.Hour

// sweepNow is the clock the age of a build is read against.
var sweepNow = time.Now

// SweepOrphans drops what a hard kill or a finished run left on the server and returns the names it dropped:
//   - a temporary template or checkpoint build older than buildMaxAge, or a template or checkpoint under its final name without the
//     completion mark: the rest of a build that never finished. A build in progress (this process or another one
//     sharing the server) holds its file lock in LockDir and is left alone, so without a LockDir no build is
//     touched;
//   - the shard and shared databases of a run in runs that has finished. A run the store does not know may belong
//     to another localci on a shared server and is left alone, and so is a run that has not finished.
//
// A database with the completion mark is a finished template or checkpoint and is never dropped here: the
// checkpoint store's LRU owns those. It keeps going after a failed drop and returns every failure.
func SweepOrphans(ctx context.Context, p *Postgres, instance string, runs []RunRef) ([]string, error) {
	names, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	finished := map[string]bool{} // prefix of a finished run's databases
	for _, r := range runs {
		if r.Finished {
			n := ShardName(instance, r.Suite, r.ID, 0)
			finished[n[:len(n)-1]] = true
		}
	}
	var dropped []string
	var errs []error
	drop := func(name string) {
		if err := p.Drop(ctx, name); err != nil {
			errs = append(errs, err)
			return
		}
		dropped = append(dropped, name)
	}
	for _, name := range names {
		if i := strings.LastIndexByte(name, '_'); i > 0 && finished[name[:i+1]] && isShardSuffix(name[i+1:]) {
			drop(name)
			continue
		}
		if !strings.HasPrefix(name, dbPrefix+"tpl_") && !strings.HasPrefix(name, dbPrefix+"ck_") || shardShaped.MatchString(name) {
			continue
		}
		base, tmp := name, false
		if m := tmpBuild.FindStringSubmatch(name); m != nil {
			base, tmp = m[1], true
			// The build lock only covers this data directory; a build of another localci on the server is
			// invisible, so a build is taken for dead only when it is far older than any build lasts.
			if !builtBefore(name, sweepNow().Add(-buildMaxAge)) {
				continue
			}
		}
		if p.LockDir == "" {
			continue
		}
		unlock, err := flock.TryLock(lockPath(p.LockDir, base))
		if errors.Is(err, flock.ErrBusy) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// The builder may have finished between the listing and the lock.
		if ok, err := p.isComplete(ctx, name); err == nil && (tmp || !ok) {
			drop(name)
		} else if err != nil {
			errs = append(errs, err)
		}
		unlock()
	}
	return dropped, errors.Join(errs...)
}

// builtBefore says whether the temporary build name (<final name>_b<nanos>) was started before t. A name whose
// time cannot be read is never taken for old.
func builtBefore(name string, t time.Time) bool {
	i := strings.LastIndex(name, "_b")
	n, err := strconv.ParseInt(name[i+2:], 10, 64)
	return i >= 0 && err == nil && time.Unix(0, n).Before(t)
}

// isShardSuffix is what follows the run's prefix in a shard (its number) or shared (s) database name.
func isShardSuffix(s string) bool {
	if s == "s" {
		return true
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

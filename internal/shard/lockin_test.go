package shard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Some filesystems (NFS, some FUSE mounts) cannot lock files. Building must still work there, with the builds of
// one process serialised, as it did before the file lock existed.
func TestLockInSerialisesInProcessWhereFileLocksAreUnsupported(t *testing.T) {
	for _, errno := range []error{syscall.ENOLCK, syscall.ENOTSUP, syscall.EOPNOTSUPP} { // darwin: EOPNOTSUPP != ENOTSUP
		old := fileLock
		fileLock = func(context.Context, string) (func(), error) { return nil, errno }
		t.Cleanup(func() { fileLock = old })

		var o onceMap
		var inside, overlap atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				unlock, err := o.lockIn(context.Background(), t.TempDir(), "build")
				if err != nil {
					t.Errorf("lockIn on a filesystem without file locks (%v) failed: %v", errno, err)
					return
				}
				if inside.Add(1) > 1 {
					overlap.Add(1)
				}
				time.Sleep(50 * time.Millisecond)
				inside.Add(-1)
				unlock()
			}()
		}
		wg.Wait()
		if overlap.Load() != 0 {
			t.Errorf("two goroutines held the lock at once (%v)", errno)
		}
	}
}

// A wait that is cancelled is an error, not a fallback to no lock.
func TestLockInReportsACancelledWait(t *testing.T) {
	old := fileLock
	fileLock = func(ctx context.Context, _ string) (func(), error) { <-ctx.Done(); return nil, ctx.Err() }
	t.Cleanup(func() { fileLock = old })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var o onceMap
	if _, err := o.lockIn(ctx, t.TempDir(), "build"); err == nil {
		t.Fatal("a cancelled wait must fail")
	}
}

// Any lock error other than "this filesystem cannot lock" fails the caller, naming the lock file, and leaves the
// in-process lock free for the next caller.
func TestLockInFailsOnALockErrorThatIsNotUnsupported(t *testing.T) {
	old := fileLock
	fileLock = func(context.Context, string) (func(), error) { return nil, syscall.EIO }
	t.Cleanup(func() { fileLock = old })
	dir := t.TempDir()
	var o onceMap
	_, err := o.lockIn(context.Background(), dir, "build")
	if !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), lockPath(dir, "build")) {
		t.Fatalf("err = %v, want EIO naming %s", err, lockPath(dir, "build"))
	}
	done := make(chan struct{})
	go func() { o.lock("build")(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a failed lockIn left the in-process lock held")
	}
}

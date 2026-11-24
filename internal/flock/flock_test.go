//go:build unix

package flock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Child process of TestLockIsHeldAcrossProcesses: tries the lock in $FLOCK_TEST_PATH for 200 ms.
func TestFlockHelperProcess(t *testing.T) {
	path := os.Getenv("FLOCK_TEST_PATH")
	if path == "" {
		t.Skip("helper process")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	unlock, err := Lock(ctx, path)
	if err != nil {
		os.Exit(3)
	}
	unlock()
}

func child(t *testing.T, path string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFlockHelperProcess$")
	cmd.Env = append(os.Environ(), "FLOCK_TEST_PATH="+path)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

func TestLockIsHeldAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "build.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if code := child(t, path); code != 3 {
		t.Fatalf("another process got the lock while it was held (exit %d)", code)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryLock on a held lock = %v, want ErrBusy", err)
	}
	unlock()
	if code := child(t, path); code != 0 {
		t.Fatalf("the released lock was not taken by the other process (exit %d)", code)
	}
}

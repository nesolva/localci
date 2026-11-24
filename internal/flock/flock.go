//go:build unix

// Package flock is an advisory file lock between localci processes. The kernel
// drops it when the holder exits, however it exits, so a kill -9 never leaves a
// lock behind.
package flock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// poll is how often a waiting Lock looks again; a wait ends with its context.
const poll = 25 * time.Millisecond

// ErrBusy is returned by TryLock when another holder has the lock.
var ErrBusy = errors.New("lock is held")

// Lock takes the lock at path (created with its directory when missing),
// waiting until it is free or ctx ends. The returned func releases it.
func Lock(ctx context.Context, path string) (func(), error) {
	for {
		unlock, err := TryLock(path)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, ErrBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// TryLock takes the lock at path when it is free and returns ErrBusy otherwise.
func TryLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	switch {
	case err == nil:
		return func() { f.Close() }, nil // closing the descriptor releases the lock
	case errors.Is(err, syscall.EWOULDBLOCK):
		f.Close()
		return nil, ErrBusy
	default:
		f.Close()
		return nil, err
	}
}

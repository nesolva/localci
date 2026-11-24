package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/flock"
	"github.com/nesolva/localci/internal/notify"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

func init() {
	register("serve", "run the dashboard, the local API, the suite runner and the pull request watcher", false, cmdServe)
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	var dataDir *string
	eff, _, code := load("serve", args, stderr, func(fs *flag.FlagSet) {
		dataDir = fs.String("data-dir", "", "where state, logs and the token live (default: the standard location)")
	})
	if eff == nil {
		return code
	}
	if *dataDir == "" {
		*dataDir = config.DataDir()
	}
	// A relative path would mean something else after a `cd`, and the socket, the lock and the database must
	// agree on one place.
	abs, err := filepath.Abs(*dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "localci: --data-dir:", err)
		return exitUsage
	}
	*dataDir = abs
	// One serve per data dir: the lock comes before anything is touched, the socket included, so a second serve
	// can never unlink the first one's socket.
	unlock, err := lockServe(*dataDir)
	if err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	defer unlock()
	if len(eff.Repos) == 0 {
		fmt.Fprintln(stderr, "localci: no `repos` in the config: pull requests are not watched, `localci run` still works")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	machine := capacity.Detect(ctx)
	err = server.Start(ctx, server.Options{
		Config:   eff,
		DataDir:  *dataDir,
		Machine:  machine,
		Log:      log.New(stderr, "", log.LstdFlags),
		Notifier: notify.New(),
		Reload: func() (*config.Effective, error) {
			return eff, nil
		},
	})
	if err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	return exitOK
}

// serveLock is the lock file of a data directory's serve; it holds the pid of the process that has it.
const serveLock = "serve.lock"

// lockServe takes the data directory's serve lock for the life of the process (the kernel drops it when the
// process exits, however it exits). When another serve has it, the error says which process.
func lockServe(dataDir string) (func(), error) {
	if err := store.Harden(dataDir); err != nil {
		return nil, fmt.Errorf("data directory %s: %w", dataDir, err)
	}
	path := filepath.Join(dataDir, serveLock)
	unlock, err := flock.TryLock(path)
	if errors.Is(err, flock.ErrBusy) {
		if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "" {
			return nil, fmt.Errorf("already running (pid %s)", strings.TrimSpace(string(b)))
		}
		return nil, errors.New("already running")
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Best effort: the lock is what counts, the pid only tells the next serve whom to look for.
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), store.FileMode)
	_ = os.Chmod(path, store.FileMode)
	return unlock, nil
}

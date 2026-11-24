package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/flock"
)

// GitError is a failed git invocation with its stderr.
type GitError struct {
	Args   []string
	Stderr string
}

// Error shows the command and stderr with the credentials of any URL in them
// (https://user:token@host) replaced, since errors reach logs and statuses.
func (e *GitError) Error() string {
	return redactURLs(fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), e.Stderr))
}

var urlUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/?#\s]*@`)

// redactURLs replaces the user and password of every URL in s.
func redactURLs(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}***@") }

// git runs git in dir (empty = current) and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", &GitError{Args: args, Stderr: strings.TrimSpace(errb.String())}
	}
	return strings.TrimSpace(out.String()), nil
}

// pathLocks serialises work on one directory inside this process.
var pathLocks sync.Map

// pathLock serialises git work on one workspace or mirror: a mutex for the
// goroutines of this process and a file lock beside the directory (in the data
// dir) for other localci processes. Holding it also entitles Lock to remove
// the lock files a killed git left in the repository.
type pathLock struct {
	mu   sync.Mutex
	path string
	file func() // releases the file lock
}

func lockFor(path string) *pathLock {
	m, _ := pathLocks.LoadOrStore(path, &pathLock{path: path})
	return m.(*pathLock)
}

// Lock waits for the directory. When the file lock cannot be taken (a read-only
// data dir) it degrades to the in-process mutex, which is what it was before.
func (l *pathLock) Lock() {
	l.mu.Lock()
	if unlock, err := flock.Lock(context.Background(), l.path+".lock"); err == nil {
		l.file = unlock
	}
	removeStaleGitLocks(l.path)
}

func (l *pathLock) Unlock() {
	if l.file != nil {
		l.file()
		l.file = nil
	}
	l.mu.Unlock()
}

// staleLockAge is how old a lock file must be before it is removed on a host without lsof to ask.
const staleLockAge = 5 * time.Minute

// removeStaleGitLocks removes the *.lock files a killed git left in the
// repository at dir (its .git, or dir itself for a bare one; never the
// working tree, where yarn.lock is a real file), so the next
// checkout does not die on "Unable to create index.lock". Call it only while
// holding the directory's lock: no other localci git runs here. A lock that
// some other process has open belongs to a live git (an editor, a hook) and is
// left alone.
func removeStaleGitLocks(dir string) {
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		// A bare mirror keeps its repository in dir itself; anything else is
		// not a repository, and a *.lock there (yarn.lock) is somebody's file.
		if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
			return
		}
		gitDir = dir
	}
	locks, _ := filepath.Glob(filepath.Join(gitDir, "*.lock"))
	for _, f := range locks {
		if gitLockInUse(f) {
			continue
		}
		os.Remove(f)
	}
}

// gitLockInUse reports whether a process may still be using the lock file f.
func gitLockInUse(f string) bool {
	out, err := osexec.Command("lsof", "-t", "--", f).Output()
	var ee *osexec.ExitError
	switch {
	case err == nil && len(bytes.TrimSpace(out)) > 0:
		return true
	case err == nil, errors.As(err, &ee) && len(bytes.TrimSpace(out)) == 0 && ee.ExitCode() == 1:
		return false // lsof ran and nobody has it open
	}
	// No lsof to ask: only a lock that has been there a while is surely left over.
	info, statErr := os.Stat(f)
	return statErr != nil || time.Since(info.ModTime()) < staleLockAge
}

// safeName turns "acme/web" into a single path element.
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteString("__")
		}
	}
	name := strings.Trim(b.String(), ".")
	if name == "" {
		return "_"
	}
	return name
}

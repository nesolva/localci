package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// gitStopDelay is how long a cancelled git has to stop before it is killed.
const gitStopDelay = 10 * time.Second

// gitGroupGrace is how long what is left of a cancelled git's group (a filter,
// a remote helper) has to stop after git itself ended, before it is killed.
const gitGroupGrace = time.Second

// git runs git in dir (empty = current) and returns trimmed stdout. Use the
// held pathLock's git instead when the repository is one of localci's own.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, _, err := runGit(ctx, dir, args...)
	return out, err
}

// runGit runs git in its own process group. A cancelled git is asked to stop,
// not killed: on SIGTERM it removes its lock files, which a kill leaves behind
// to fail the next git in that repository. The whole group gets the signal, and
// once git has ended what is left of the group is stopped too, so no helper
// outlives it. gone reports that git was cancelled and nothing of its group is
// left: any lock file it left is dead.
func runGit(ctx context.Context, dir string, args ...string) (out string, gone bool, err error) {
	cmd := osexec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &errb
	setGroup(cmd)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = gitStopDelay
	err = cmd.Run()
	if ctx.Err() != nil && cmd.Process != nil {
		gone = endGroup(cmd.Process.Pid)
	}
	// ErrWaitDelay: git succeeded, but something it left running (an ssh
	// connection kept open for reuse) still holds its output.
	if err != nil && !(errors.Is(err, osexec.ErrWaitDelay) && ctx.Err() == nil) {
		if ctx.Err() != nil {
			return "", gone, ctx.Err()
		}
		return "", false, &GitError{Args: args, Stderr: strings.TrimSpace(errb.String())}
	}
	return strings.TrimSpace(stdout.String()), gone, nil
}

// endGroup stops what is left of the process group pgid and reports whether it
// is gone. A member killed a moment ago can still be a zombie in the group until
// its new parent collects it, so this waits a little for that.
func endGroup(pgid int) bool {
	stopGroup(pgid, gitGroupGrace)
	for deadline := time.Now().Add(time.Second); groupExists(pgid) && time.Now().Before(deadline); {
		reapAdopted(pgid, nil)
		time.Sleep(20 * time.Millisecond)
	}
	return !groupExists(pgid)
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
	// dead is set, while the lock is held, when a git cancelled here ended with
	// its whole process group: every lock file in the repository is then left
	// over, whatever its age.
	dead bool
}

// git runs git for the holder of l. It must be held.
func (l *pathLock) git(ctx context.Context, dir string, args ...string) (string, error) {
	out, gone, err := runGit(ctx, dir, args...)
	if gone {
		l.dead = true
	}
	return out, err
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
	removeStaleGitLocks(l.path, false)
}

// Unlock first clears what a git cancelled under the lock left, so the next
// holder, in this process or another, starts clean. Without the file lock
// another localci process may be using the repository, so that waits for the
// next Lock and its stricter rule.
func (l *pathLock) Unlock() {
	if l.dead && l.file != nil {
		removeStaleGitLocks(l.path, true)
	}
	l.dead = false
	if l.file != nil {
		l.file()
		l.file = nil
	}
	l.mu.Unlock()
}

// removeStaleGitLocks removes the lock files a killed git left in the
// repository at dir (its .git, or dir itself for a bare one; never the
// working tree, where yarn.lock is a real file), so the next git there does
// not die on "Unable to create index.lock" or "cannot lock ref". Those are the
// *.lock files at the top of the git dir and anywhere under refs/ (a ref's
// name never ends in .lock). Call it only while holding the directory's lock:
// no other localci git runs here. A lock that some other process has open
// belongs to a live git (an editor, a hook) and is left alone. When dead (a git
// cancelled under the lock ended with its whole group), a lock nobody is known
// to hold goes whatever its age; otherwise a host that cannot tell who holds it
// keeps a young one.
func removeStaleGitLocks(dir string, dead bool) {
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
	filepath.WalkDir(filepath.Join(gitDir, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(p, ".lock") {
			locks = append(locks, p)
		}
		return nil
	})
	for _, f := range locks {
		held, known := lockHolder(f)
		if held || (!known && !dead && young(f)) {
			continue
		}
		os.Remove(f)
	}
}

// lsofCmd and procRoot are what lockHolder asks; a test points them at nothing to act as a host without them.
var lsofCmd, procRoot = "lsof", "/proc"

// lockHolder reports whether a process has the lock file f open, and whether
// that could be found out: by lsof, or on Linux by the open files under /proc.
func lockHolder(f string) (held, known bool) {
	out, err := osexec.Command(lsofCmd, "-t", "--", f).Output()
	var ee *osexec.ExitError
	switch {
	case err == nil && len(bytes.TrimSpace(out)) > 0:
		return true, true
	case err == nil, errors.As(err, &ee) && len(bytes.TrimSpace(out)) == 0 && ee.ExitCode() == 1:
		return false, true // lsof ran and nobody has it open
	}
	return procHolder(f)
}

// procHolder looks for f among the open files of every process under procRoot.
// It knows only when it could read this process's own open files, and it sees
// only the processes of this user: git runs as localci's own user.
func procHolder(f string) (held, known bool) {
	if real, err := filepath.EvalSymlinks(f); err == nil {
		f = real
	}
	if abs, err := filepath.Abs(f); err == nil {
		f = abs
	}
	procs, err := os.ReadDir(procRoot)
	if err != nil {
		return false, false
	}
	self := strconv.Itoa(os.Getpid())
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join(procRoot, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue // another user's process, or one that just ended
		}
		if p.Name() == self {
			known = true
		}
		for _, fd := range fds {
			if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && target == f {
				return true, true
			}
		}
	}
	return false, known
}

// staleLockAge is how old a lock file must be before it is surely left over, on a host that cannot tell who holds it.
const staleLockAge = 5 * time.Minute

// young reports whether the lock file f is too recent to be surely left over (or cannot be read).
func young(f string) bool {
	info, err := os.Stat(f)
	return err != nil || time.Since(info.ModTime()) < staleLockAge
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

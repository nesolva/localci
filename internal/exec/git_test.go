package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var bg = context.Background()

// run executes git in dir for test setup and fails the test on error.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newOrigin makes a repo "acme/web" with one commit on main.
func newOrigin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "app.txt"), "one\ntwo\nthree\n")
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func newMirror(t *testing.T, origin string) *Mirror {
	t.Helper()
	m, err := EnsureMirror(bg, t.TempDir(), "acme/web", origin)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// pull publishes branch as pull request n; clean also publishes the merge ref
// the forge creates when the pull request merges into main.
func pull(t *testing.T, origin string, n, branch string, clean bool) {
	t.Helper()
	run(t, origin, "update-ref", "refs/pull/"+n+"/head", branch)
	if clean {
		run(t, origin, "checkout", "-q", "-b", "merge-"+n, "main")
		run(t, origin, "merge", "-q", "--no-ff", "-m", "merge", branch)
		run(t, origin, "update-ref", "refs/pull/"+n+"/merge", "HEAD")
		run(t, origin, "checkout", "-q", "main")
	}
}

func TestPullMergeRefAndConflict(t *testing.T) {
	origin := newOrigin(t)

	// PR 1 edits line three, main later edits line one: merges cleanly.
	run(t, origin, "checkout", "-q", "-b", "feat-clean")
	write(t, filepath.Join(origin, "app.txt"), "one\ntwo\nTHREE\n")
	run(t, origin, "commit", "-q", "-am", "pr1")
	run(t, origin, "checkout", "-q", "main")
	write(t, filepath.Join(origin, "app.txt"), "ONE\ntwo\nthree\n")
	run(t, origin, "commit", "-q", "-am", "main moves")
	pull(t, origin, "1", "feat-clean", true)

	// PR 2 edits line one too: no merge ref.
	run(t, origin, "checkout", "-q", "-b", "feat-conflict", "HEAD~1")
	write(t, filepath.Join(origin, "app.txt"), "uno\ntwo\nthree\n")
	run(t, origin, "commit", "-q", "-am", "pr2")
	run(t, origin, "checkout", "-q", "main")
	pull(t, origin, "2", "feat-conflict", false)

	m := newMirror(t, origin)

	got, err := m.FetchPull(bg, 1, "main")
	if err != nil {
		t.Fatal(err)
	}
	ws := OpenWorkspace(t.TempDir(), "e2e", "acme/web")
	if err := ws.Checkout(bg, m, got.Merge, nil); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(ws.Dir, "app.txt"))
	if string(body) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("PR 1 must run on the merge commit (both changes), got %q", body)
	}

	_, err = m.FetchPull(bg, 2, "main")
	var mc *MergeConflictError
	if !asMergeConflict(err, &mc) || err.Error() != "merge conflict with main" {
		t.Fatalf("PR 2: want merge conflict with main, got %v", err)
	}

	// PR 1 turns conflicting later: the merge commit fetched earlier must not be reused.
	run(t, origin, "update-ref", "-d", "refs/pull/1/merge")
	if _, err := m.FetchPull(bg, 1, "main"); !asMergeConflict(err, &mc) {
		t.Fatalf("stale merge ref must not survive, got %v", err)
	}
}

func asMergeConflict(err error, target **MergeConflictError) bool {
	return errors.As(err, target)
}

func TestSnapshotLeavesCheckoutUntouched(t *testing.T) {
	origin := newOrigin(t)
	m := newMirror(t, origin)

	write(t, filepath.Join(origin, "app.txt"), "one\nEDITED\nthree\n") // tracked, unstaged
	write(t, filepath.Join(origin, "new.txt"), "untracked\n")
	write(t, filepath.Join(origin, ".gitignore"), "ignored.log\n")
	write(t, filepath.Join(origin, "ignored.log"), "x\n")
	before := run(t, origin, "status", "--porcelain")

	s, err := m.Snapshot(bg, origin)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Dirty {
		t.Fatal("snapshot of a modified tree must be dirty")
	}
	if got := run(t, m.Dir, "show", s.SHA+":app.txt"); got != "one\nEDITED\nthree" {
		t.Fatalf("uncommitted tracked change missing from the snapshot: %q", got)
	}
	if len(s.Untracked) != 2 || s.Untracked[0] != ".gitignore" || s.Untracked[1] != "new.txt" {
		// .gitignore is untracked here; ignored.log is ignored and not reported
		t.Fatalf("untracked files: %v", s.Untracked)
	}
	if after := run(t, origin, "status", "--porcelain"); after != before {
		t.Fatalf("checkout changed:\nbefore %q\nafter  %q", before, after)
	}

	// A clean checkout yields HEAD.
	run(t, origin, "checkout", "-q", "app.txt")
	run(t, origin, "clean", "-fdxq")
	s, err = m.Snapshot(bg, origin)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dirty || s.SHA != run(t, origin, "rev-parse", "HEAD") {
		t.Fatalf("clean checkout: %+v", s)
	}
}

// A URL, path or ref that starts with "-" is an argument, never a git option.
func TestGitArgumentsStartingWithADashAreNotOptions(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	origin := newOrigin(t)
	if err := os.Rename(origin, filepath.Join(work, "-origin")); err != nil {
		t.Fatal(err)
	}
	m, err := EnsureMirror(bg, t.TempDir(), "acme/web", "-origin")
	if err != nil {
		t.Fatalf("a repository path starting with a dash: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	err = m.Fetch(bg, "--upload-pack=touch "+marker+" #")
	if _, statErr := os.Stat(marker); statErr == nil || err == nil {
		t.Fatalf("a refspec became --upload-pack (err %v)", err)
	}
	write(t, filepath.Join(work, "-origin", "app.txt"), "edited\n")
	if _, err := m.Snapshot(bg, "-origin"); err != nil {
		t.Fatalf("snapshot of a checkout whose path starts with a dash: %v", err)
	}
}

func TestGitErrorsDoNotShowCredentials(t *testing.T) {
	_, err := EnsureMirror(bg, t.TempDir(), "acme/web", "https://bot:s3cret-token@127.0.0.1:1/acme/web.git")
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("err = %v, want a GitError", err)
	}
	if msg := err.Error(); strings.Contains(msg, "s3cret-token") || strings.Contains(msg, "bot:") || !strings.Contains(msg, "127.0.0.1:1/acme/web.git") {
		t.Fatalf("error = %q", msg)
	}
}

func TestWarmWorkspaceKeepsOnlyKeptPaths(t *testing.T) {
	origin := newOrigin(t)
	first := run(t, origin, "rev-parse", "HEAD")
	write(t, filepath.Join(origin, "app.txt"), "v2\n")
	run(t, origin, "commit", "-q", "-am", "v2")
	second := run(t, origin, "rev-parse", "HEAD")
	m := newMirror(t, origin)
	if err := m.Fetch(bg, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatal(err)
	}

	ws := OpenWorkspace(t.TempDir(), "e2e", "acme/web")
	keep := []string{"node_modules/"}
	if err := ws.Checkout(bg, m, first, keep); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ws.Dir, "node_modules", "dep", "index.js"), "installed\n")
	write(t, filepath.Join(ws.Dir, "stray.txt"), "left by the last run\n")
	write(t, filepath.Join(ws.Dir, "app.txt"), "dirtied by the last run\n")

	if err := ws.Checkout(bg, m, second, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "node_modules", "dep", "index.js")); err != nil {
		t.Fatalf("kept node_modules must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "stray.txt")); err == nil {
		t.Fatal("other untracked files must be removed")
	}
	if b, _ := os.ReadFile(filepath.Join(ws.Dir, "app.txt")); string(b) != "v2\n" {
		t.Fatalf("workspace must be at the new sha with a clean tree, got %q", b)
	}
}

// A git killed mid-checkout or mid-fetch leaves its lock files behind, and every later git of that repository dies on
// them: .git/index.lock in a workspace, refs/heads/<branch>.lock (or refs/pull/N/head.lock) in a mirror. The
// repository's own lock files go, nested ref locks included; a *.lock in the working tree is a tracked file of the
// project.
func TestCheckoutClearsLockFilesALeftByAKilledGit(t *testing.T) {
	origin := newOrigin(t)
	write(t, filepath.Join(origin, "yarn.lock"), "# lockfile\n")
	run(t, origin, "add", ".")
	run(t, origin, "commit", "-q", "-m", "lockfile")
	head := run(t, origin, "rev-parse", "HEAD")
	m := newMirror(t, origin)
	if err := m.Fetch(bg, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatal(err)
	}
	ws := OpenWorkspace(t.TempDir(), "e2e", "acme/web")
	if err := ws.Checkout(bg, m, head, nil); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-time.Hour) // for a host without lsof, which then goes by age
	leave := func(lock string) {
		write(t, lock, "")
		if err := os.Chtimes(lock, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"index.lock", "HEAD.lock", "refs/heads/main.lock"} {
		leave(filepath.Join(ws.Dir, ".git", name))
	}
	if err := ws.Checkout(bg, m, head, nil); err != nil {
		t.Fatalf("checkout after a killed git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, ".git", "refs", "heads", "main.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a ref lock left by a killed git is still there (stat: %v)", err)
	}
	if _, err := os.Stat(filepath.Join(ws.Dir, "yarn.lock")); err != nil {
		t.Fatalf("a tracked *.lock file of the project must survive: %v", err)
	}

	leave(filepath.Join(m.Dir, "refs", "heads", "main.lock"))
	write(t, filepath.Join(origin, "app.txt"), "moved\n")
	run(t, origin, "commit", "-q", "-am", "moved")
	if err := m.Fetch(bg, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatalf("fetch after a killed fetch left refs/heads/main.lock: %v", err)
	}
}

// A cancelled run's git must take its lock files with it. A host without lsof keeps a young lock for minutes, so one
// left behind fails the run that superseded this one.
func TestCancelledCheckoutLeavesNoLockFile(t *testing.T) {
	origin := newOrigin(t)
	write(t, filepath.Join(origin, ".gitattributes"), "app.txt filter=hold\n")
	write(t, filepath.Join(origin, "app.txt"), "v1\n")
	run(t, origin, "add", ".")
	run(t, origin, "commit", "-q", "-m", "one")
	first := run(t, origin, "rev-parse", "HEAD")
	write(t, filepath.Join(origin, "app.txt"), "v2\n")
	run(t, origin, "commit", "-q", "-am", "two")
	second := run(t, origin, "rev-parse", "HEAD")
	m := newMirror(t, origin)
	if err := m.Fetch(bg, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatal(err)
	}
	ws := OpenWorkspace(t.TempDir(), "check", "acme/web")
	if err := ws.Checkout(bg, m, first, nil); err != nil {
		t.Fatal(err)
	}

	noLsof(t)

	// The filter holds the next checkout while git has the index locked. It
	// lasts while its marker does, so removing the temp dir ends it.
	tmp := t.TempDir()
	held, pidFile := filepath.Join(tmp, "held"), filepath.Join(tmp, "pid")
	run(t, ws.Dir, "config", "filter.hold.smudge",
		"exec 2>/dev/null; echo $$ > '"+pidFile+"'; touch '"+held+"'; while [ -e '"+held+"' ]; do sleep 0.05; done; cat")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ws.Checkout(ctx, m, second, nil) }()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(held); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("the checkout never reached the filter")
		}
	}
	lock := filepath.Join(ws.Dir, ".git", "index.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the held checkout has no index lock: %v", err)
	}
	// What a git killed before it could clean up leaves: a young lock beside the index and one under refs/.
	left := []string{filepath.Join(ws.Dir, ".git", "packed-refs.lock"), filepath.Join(ws.Dir, ".git", "refs", "heads", "x.lock")}
	for _, f := range left {
		write(t, f, "")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled checkout = %v, want %v", err, context.Canceled)
	}
	for _, f := range append(left, lock) {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the cancelled checkout left %s (stat: %v)", strings.TrimPrefix(f, ws.Dir), err)
		}
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for deadline := time.Now().Add(5 * time.Second); processAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled git's filter (pid %d) is still running", pid)
		}
	}
	run(t, ws.Dir, "config", "--unset", "filter.hold.smudge")
	if err := ws.Checkout(bg, m, second, nil); err != nil {
		t.Fatalf("the checkout after the cancelled one: %v", err)
	}
}

// noLsof makes this test a host without lsof (and, on Linux, without /proc to stand in for it).
func noLsof(t *testing.T) {
	t.Helper()
	lsof, proc := lsofCmd, procRoot
	lsofCmd, procRoot = filepath.Join(t.TempDir(), "no-lsof"), filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { lsofCmd, procRoot = lsof, proc })
}

// processAlive reports whether pid runs (a zombie counts as gone).
func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := osexec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// A git killed during the workspace's first `git init` (a run superseded right after it started) leaves a .git that
// is not a repository yet. The next run must complete it, or every later checkout of that workspace fails.
func TestCheckoutCompletesAHalfMadeRepository(t *testing.T) {
	origin := newOrigin(t)
	write(t, filepath.Join(origin, "app.txt"), "v1\n")
	run(t, origin, "add", ".")
	run(t, origin, "commit", "-q", "-m", "one")
	head := run(t, origin, "rev-parse", "HEAD")
	m := newMirror(t, origin)
	if err := m.Fetch(bg, "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatal(err)
	}
	// What a killed `git init` has been seen to leave: some directories, no HEAD.
	for name, left := range map[string][]string{
		"only objects":         {"objects"},
		"templates, no HEAD":   {"hooks", "info", "objects"},
		"an empty .git folder": {""},
	} {
		t.Run(name, func(t *testing.T) {
			ws := OpenWorkspace(t.TempDir(), "check", "acme/web")
			for _, d := range left {
				if err := os.MkdirAll(filepath.Join(ws.Dir, ".git", d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := ws.Checkout(bg, m, head, nil); err != nil {
				t.Fatalf("checkout after a killed git init: %v", err)
			}
			if b, _ := os.ReadFile(filepath.Join(ws.Dir, "app.txt")); string(b) != "v1\n" {
				t.Fatalf("app.txt = %q, want the commit's", b)
			}
		})
	}
}

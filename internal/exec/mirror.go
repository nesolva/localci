package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MergeConflictError means the pull request has no merge ref: it does not
// merge cleanly into its base. It is an infrastructure failure, not a test
// failure.
type MergeConflictError struct{ Base string }

func (e *MergeConflictError) Error() string { return "merge conflict with " + e.Base }

// Infrastructure marks the error as not caused by the code under test.
func (e *MergeConflictError) Infrastructure() bool { return true }

// Mirror is a bare mirror clone of one repo under the data dir. Checkouts
// borrow its objects, so a head is fetched once however many workspaces use it.
type Mirror struct {
	Dir string // the bare repository
	URL string // where it is fetched from
}

// MirrorDir is where the mirror of repo lives under dataDir.
func MirrorDir(dataDir, repo string) string {
	return filepath.Join(dataDir, "mirrors", safeName(repo)+".git")
}

// EnsureMirror returns the mirror of repo ("owner/name") under dataDir,
// cloning it from url on first use. Automatic gc and maintenance are off: the
// workspaces borrow its objects and must never lose them.
func EnsureMirror(ctx context.Context, dataDir, repo, url string) (*Mirror, error) {
	dir := MirrorDir(dataDir, repo)
	mu := lockFor(dir)
	mu.Lock()
	defer mu.Unlock()
	m := &Mirror{Dir: dir, URL: url}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return m, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	tmp := dir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if _, err := git(ctx, "", "clone", "--quiet", "--mirror", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "--", url, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	return m, nil
}

// Fetch fetches explicit refspecs from the origin.
func (m *Mirror) Fetch(ctx context.Context, refspecs ...string) error {
	mu := lockFor(m.Dir)
	mu.Lock()
	defer mu.Unlock()
	args := append([]string{"-C", m.Dir, "fetch", "--quiet", "--no-tags", "--", "origin"}, refspecs...)
	_, err := git(ctx, "", args...)
	return err
}

// FetchBranch fetches a branch and returns the sha it points at.
func (m *Mirror) FetchBranch(ctx context.Context, branch string) (string, error) {
	ref := "refs/heads/" + branch
	if err := m.Fetch(ctx, "+"+ref+":"+ref); err != nil {
		return "", err
	}
	return m.rev(ctx, ref)
}

// PullRef is the head and the merge commit of a pull request. MergeHead is the
// head the merge commit merges (its second parent): GitHub rebuilds the merge
// ref lazily, so right after a push it can differ from Head.
type PullRef struct{ Head, Merge, MergeHead string }

// FetchPull fetches refs/pull/N/head and refs/pull/N/merge. A missing merge
// ref (and a stale local copy of it) means the pull request conflicts with
// base: that is a *MergeConflictError.
func (m *Mirror) FetchPull(ctx context.Context, n int, base string) (PullRef, error) {
	head := fmt.Sprintf("refs/pull/%d/head", n)
	merge := fmt.Sprintf("refs/pull/%d/merge", n)
	if err := m.Fetch(ctx, "+"+head+":"+head); err != nil {
		return PullRef{}, err
	}
	if err := m.Fetch(ctx, "+"+merge+":"+merge); err != nil {
		var ge *GitError
		if errors.As(err, &ge) && strings.Contains(ge.Stderr, "couldn't find remote ref") {
			// Forget the merge commit of an earlier head, or it would be used.
			mu := lockFor(m.Dir)
			mu.Lock()
			git(ctx, m.Dir, "update-ref", "-d", merge)
			mu.Unlock()
			return PullRef{}, &MergeConflictError{Base: base}
		}
		return PullRef{}, err
	}
	h, err := m.rev(ctx, head)
	if err != nil {
		return PullRef{}, err
	}
	mg, err := m.rev(ctx, merge)
	if err != nil {
		return PullRef{}, err
	}
	mh, _ := m.rev(ctx, mg+"^2") // empty when it is not a merge commit
	return PullRef{Head: h, Merge: mg, MergeHead: mh}, nil
}

func (m *Mirror) rev(ctx context.Context, ref string) (string, error) {
	return git(ctx, m.Dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

package exec

import (
	"context"
	"path/filepath"
	"strings"
)

// Snapshot is a checkout's working tree as a commit in a mirror.
type Snapshot struct {
	SHA       string
	Dirty     bool     // the commit holds uncommitted tracked changes
	Untracked []string // files the snapshot left out (untracked, not ignored)
}

// Snapshot records the tracked state of the checkout in dir, uncommitted
// changes included, as a commit fetched into the mirror from the local path.
// The checkout is not touched: no ref, no index, no file changes. A clean
// checkout yields its HEAD.
func (m *Mirror) Snapshot(ctx context.Context, dir string) (Snapshot, error) {
	var s Snapshot
	sha, err := git(ctx, dir, "-c", "user.name=localci", "-c", "user.email=localci@localhost", "stash", "create")
	if err != nil {
		return s, err
	}
	if sha == "" {
		if sha, err = git(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}"); err != nil {
			return s, err
		}
	} else {
		s.Dirty = true
	}
	s.SHA = sha

	// The stash commit is on no ref, so the serving side must allow asking for it.
	ref := "refs/localci/snapshots/" + sha
	// The fetch runs in the mirror, so the checkout's path must be absolute,
	// which also keeps git from reading a path that starts with "-" as an option.
	src, err := filepath.Abs(dir)
	if err != nil {
		return s, err
	}
	mu := lockFor(m.Dir)
	mu.Lock()
	_, err = git(ctx, m.Dir, "fetch", "--quiet", "--no-tags",
		"--upload-pack", "git -c uploadpack.allowAnySHA1InWant=true upload-pack",
		"--", src, "+"+sha+":"+ref)
	mu.Unlock()
	if err != nil {
		return s, err
	}

	out, err := git(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return s, err
	}
	if out != "" {
		s.Untracked = strings.Split(out, "\n")
	}
	return s, nil
}

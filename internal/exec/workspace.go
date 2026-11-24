package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace is the one persistent checkout of a repo for a suite. Moving it to
// another sha keeps the paths the suite asks to keep (prepare outputs such as
// installed dependencies), so a warm workspace skips most of the prepare work.
// A workspace is used by one run at a time; the caller guarantees that.
type Workspace struct{ Dir string }

// OpenWorkspace returns the workspace of (suite, repo) under root.
func OpenWorkspace(root, suite, repo string) *Workspace {
	return &Workspace{Dir: filepath.Join(root, safeName(suite), safeName(repo))}
}

// Checkout moves the workspace to sha, taken from the mirror. Everything
// untracked or ignored is removed except paths matching keep, which are
// gitignore-style patterns ("node_modules/", "**/.cache").
func (w *Workspace) Checkout(ctx context.Context, m *Mirror, sha string, keep []string) error {
	// Another localci process may use the same workspace; this also clears the
	// lock files of a git that was killed here.
	mu := lockFor(w.Dir)
	mu.Lock()
	defer mu.Unlock()
	if err := w.init(ctx, m); err != nil {
		return err
	}
	// git before 2.45 has no --end-of-options for checkout (a worker machine may have an older git), so a value
	// that could be read as an option is refused here instead.
	if strings.HasPrefix(sha, "-") {
		return fmt.Errorf("checkout: %q is not a commit", sha)
	}
	if _, err := git(ctx, w.Dir, "checkout", "--quiet", "--detach", "--force", sha); err != nil {
		return err
	}
	args := []string{"clean", "-ffdx", "--quiet"}
	for _, k := range keep {
		args = append(args, "-e", k)
	}
	_, err := git(ctx, w.Dir, args...)
	return err
}

// init creates the repository on first use and points it at the mirror's objects.
func (w *Workspace) init(ctx context.Context, m *Mirror) error {
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(w.Dir, ".git")); err != nil {
		if _, err := git(ctx, w.Dir, "init", "--quiet"); err != nil {
			return err
		}
		for _, kv := range [][2]string{{"gc.auto", "0"}, {"maintenance.auto", "false"}} {
			if _, err := git(ctx, w.Dir, "config", kv[0], kv[1]); err != nil {
				return err
			}
		}
	}
	alt := filepath.Join(w.Dir, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
		return err
	}
	return os.WriteFile(alt, []byte(filepath.Join(m.Dir, "objects")+"\n"), 0o644)
}

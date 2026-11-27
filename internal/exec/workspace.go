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
	if err := w.init(ctx, mu, m); err != nil {
		return err
	}
	// git before 2.45 has no --end-of-options for checkout (a worker machine may have an older git), so a value
	// that could be read as an option is refused here instead.
	if strings.HasPrefix(sha, "-") {
		return fmt.Errorf("checkout: %q is not a commit", sha)
	}
	if _, err := mu.git(ctx, w.Dir, "checkout", "--quiet", "--detach", "--force", sha); err != nil {
		return err
	}
	args := []string{"clean", "-ffdx", "--quiet"}
	for _, k := range keep {
		args = append(args, "-e", k)
	}
	_, err := mu.git(ctx, w.Dir, args...)
	return err
}

// init creates the repository and points it at the mirror's objects. All of it
// runs every time: a git killed here (a superseded or cancelled run) can leave a
// .git that is not a repository yet, or one without its settings, so a .git that
// exists proves nothing. `git init` completes it and is safe on a complete one.
func (w *Workspace) init(ctx context.Context, mu *pathLock, m *Mirror) error {
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "gc.auto", "0"},
		{"config", "maintenance.auto", "false"},
	} {
		if _, err := mu.git(ctx, w.Dir, args...); err != nil {
			return err
		}
	}
	alt := filepath.Join(w.Dir, ".git", "objects", "info", "alternates")
	if err := os.MkdirAll(filepath.Dir(alt), 0o755); err != nil {
		return err
	}
	return os.WriteFile(alt, []byte(filepath.Join(m.Dir, "objects")+"\n"), 0o644)
}

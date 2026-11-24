package suite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/nesolva/localci/internal/exec"
)

// checkout moves every repo's workspace to the head the run tests.
func (ex *execution) checkout() error {
	keep := map[string][]string{}
	for _, p := range ex.suite.Prepare {
		keep[p.Repo] = append(keep[p.Repo], p.Keep...)
	}
	heads := map[string]Head{}
	for _, h := range ex.j.req.Heads {
		heads[h.Repo] = h
	}
	for _, repo := range ex.suite.Repos {
		m, err := exec.EnsureMirror(ex.ctx, ex.r.d.DataDir, repo, ex.r.d.RepoURL(repo))
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		sha, err := resolveHead(ex.ctx, m, heads[repo])
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		ws := exec.OpenWorkspace(filepath.Join(ex.r.d.DataDir, "workspaces"), ex.suite.Name, repo)
		if err := ws.Checkout(ex.ctx, m, sha, keep[repo]); err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		ex.dirs[repo] = ws.Dir
		ex.mu.Lock()
		ex.mirrors[repo], ex.shas[repo] = m, sha
		ex.mu.Unlock()
		ex.emit(0, "checkout", "", "", map[string]string{"repo": repo, "sha": sha})
	}
	return nil
}

// resolveHead makes sure the mirror holds the commit h names and returns its sha.
func resolveHead(ctx context.Context, m *exec.Mirror, h Head) (string, error) {
	has := func(sha string) bool {
		_, err := gitOut(ctx, m.Dir, "cat-file", "-e", sha+"^{commit}")
		return err == nil
	}
	switch {
	case h.Dir != "":
		snap, err := m.Snapshot(ctx, h.Dir)
		return snap.SHA, err
	case h.PR > 0:
		ref := fmt.Sprintf("refs/pull/%d/head", h.PR)
		if h.SHA == "" || !has(h.SHA) {
			if err := m.Fetch(ctx, "+"+ref+":"+ref); err != nil {
				return "", err
			}
		}
		if h.SHA != "" {
			return h.SHA, nil
		}
		return gitOut(ctx, m.Dir, "rev-parse", "--verify", ref+"^{commit}")
	case h.Branch != "":
		sha, err := m.FetchBranch(ctx, h.Branch)
		if err != nil {
			return "", err
		}
		if h.SHA != "" {
			if !has(h.SHA) {
				return "", fmt.Errorf("commit %s is not on branch %s", h.SHA, h.Branch)
			}
			return h.SHA, nil
		}
		return sha, nil
	case h.SHA != "":
		if !has(h.SHA) {
			if err := m.Fetch(ctx, h.SHA); err != nil {
				return "", err
			}
		}
		return h.SHA, nil
	}
	if err := m.Fetch(ctx); err != nil {
		return "", err
	}
	return gitOut(ctx, m.Dir, "rev-parse", "--verify", "HEAD^{commit}")
}

// prepare runs the suite's prepare commands that need to run.
func (ex *execution) prepare() error {
	dir := filepath.Join(ex.state, "prepare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for i, p := range ex.suite.Prepare {
		ws := ex.dirs[p.Repo]
		mark := filepath.Join(dir, strconv.Itoa(i)+"-"+slug(p.Repo))
		key, err := prepareKey(ws, p)
		if err != nil {
			return fmt.Errorf("prepare %d: %w", i+1, err)
		}
		// Skipping needs a key to compare and kept outputs to rely on: without them a checkout would have
		// wiped what the command made.
		if len(p.CacheKey) > 0 && len(p.Keep) > 0 && keptExist(ws, p.Keep) {
			if old, err := os.ReadFile(mark); err == nil && string(old) == key {
				ex.emit(0, "prepare", "", "", map[string]any{"repo": p.Repo, "run": p.Run, "skipped": true})
				continue
			}
		}
		os.Remove(mark)
		ex.emit(0, "prepare", "", "", map[string]any{"repo": p.Repo, "run": p.Run, "skipped": false})
		log := filepath.Join(ex.logDir, fmt.Sprintf("prepare-%d.log", i+1))
		res, err := exec.Run(ex.ctx, exec.Spec{Cmd: p.Run, Dir: ws, LogPath: log, KillGrace: 5 * time.Second})
		if err != nil {
			return err
		}
		if !res.OK() {
			if ex.ctx.Err() != nil {
				return ex.ctx.Err()
			}
			return fmt.Errorf("prepare failed (exit %d): %s\n%s", res.ExitCode, p.Run, strings.Join(tail(log, 8), "\n"))
		}
		if err := os.WriteFile(mark, []byte(key), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prepareKey hashes the command and the files its cache_key patterns match.
func prepareKey(dir string, p Prepare) (string, error) {
	files, err := globFiles(dir, p.CacheKey)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00", p.Run)
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// globFiles returns the files (not directories) under dir that match any pattern, sorted.
func globFiles(dir string, patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	root := os.DirFS(dir)
	for _, pat := range patterns {
		matches, err := doublestar.Glob(root, pat)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", pat, err)
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			if st, err := fs.Stat(root, m); err != nil || st.IsDir() {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	slices.Sort(out)
	return out, nil
}

// keptExist reports whether every keep path still has something in the workspace.
func keptExist(dir string, keep []string) bool {
	root := os.DirFS(dir)
	for _, k := range keep {
		k = strings.TrimSuffix(strings.TrimPrefix(k, "/"), "/")
		if ms, err := doublestar.Glob(root, k); err != nil || len(ms) == 0 {
			return false
		}
	}
	return true
}

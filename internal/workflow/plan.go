package workflow

import (
	"bytes"
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Plan returns the checks GitHub Actions would run for ev on commit sha of
// the repository checked out in repoDir. Workflow files are read from the
// commit, not from the working tree.
func Plan(repoDir, sha string, ev Event) ([]Check, error) {
	r, err := PlanDetailed(repoDir, sha, ev)
	return r.Checks, err
}

// PlanDetailed is Plan plus the reasons workflows and jobs were skipped.
func PlanDetailed(repoDir, sha string, ev Event) (Result, error) {
	full, err := git(repoDir, "rev-parse", "--verify", "--quiet", "--end-of-options", sha+"^{commit}")
	if err != nil {
		return Result{}, fmt.Errorf("commit %q not found in %s", sha, repoDir)
	}
	sha = strings.TrimSpace(full)

	if ev.Repo == "" {
		ev.Repo = repoName(repoDir)
	}
	if ev.Files == nil && ev.BaseRef != "" {
		files, err := ChangedFiles(repoDir, ev.BaseRef, sha)
		if err != nil {
			return Result{}, err
		}
		ev.Files = files
	}

	list, err := git(repoDir, "ls-tree", "-r", "--name-only", sha, "--", ".github/workflows")
	if err != nil {
		return Result{}, fmt.Errorf("listing workflows at %s: %w", sha, err)
	}
	files := map[string][]byte{}
	for _, p := range strings.Split(list, "\n") {
		if workflowFile(p) {
			b, err := git(repoDir, "show", sha+":"+p)
			if err != nil {
				return Result{}, fmt.Errorf("reading %s at %s: %w", p, sha, err)
			}
			files[p] = []byte(b)
		}
	}
	return PlanWorkflows(sha, ev, files), nil
}

// PlanWorkflows plans already-read workflow files (keyed by repo path), for
// callers that have no git checkout at hand.
func PlanWorkflows(sha string, ev Event, files map[string][]byte) Result {
	if ev.Name == "" {
		ev.Name = "pull_request"
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	res := Result{Checks: []Check{}, Skipped: []Skip{}}
	for _, p := range paths {
		c, s := planWorkflow(ev.Repo, p, files[p], ev, sha)
		res.Checks = append(res.Checks, c...)
		res.Skipped = append(res.Skipped, s...)
	}
	return res
}

// ChangedFiles lists the paths changed between base and head, the way a pull
// request diff does (relative to their merge base).
func ChangedFiles(repoDir, base, head string) ([]string, error) {
	out, err := git(repoDir, "diff", "--name-only", "--no-renames", "-z", "--end-of-options", base+"..."+head)
	if err != nil {
		return nil, fmt.Errorf("diff %s...%s: %w", base, head, err)
	}
	files := []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// repoName derives owner/name from the origin remote, else the directory name.
func repoName(dir string) string {
	if url, err := git(dir, "remote", "get-url", "origin"); err == nil {
		url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
		url = strings.ReplaceAll(url, ":", "/")
		parts := strings.Split(url, "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	return path.Base(filepath.ToSlash(abs))
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

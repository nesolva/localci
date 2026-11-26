package suite

import (
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// String is the filter as a person would type it, for the reason of a run it left with nothing to run.
func (f Filter) String() string {
	var parts []string
	if f.Grep != "" {
		parts = append(parts, "--grep "+strconv.Quote(f.Grep))
	}
	if f.FailedOf != "" {
		parts = append(parts, "--failed "+f.FailedOf)
	}
	if len(f.IDs) > 0 {
		parts = append(parts, "ids "+strings.Join(f.IDs, " "))
	}
	return strings.Join(append(parts, f.Files...), " ")
}

// fileFilter holds the files and directories a run was asked for, and where the listed specs are in the test repo.
type fileFilter struct {
	typed []string            // as asked for, made relative to the repo when it was an absolute path in a checkout
	paths map[string][]string // listed file -> its path from the root of the test repo
}

// newFileFilter prepares the match of files against the listed tests. The framework lists files relative to its own
// test directory (Playwright's testDir), which it does not report, so the tracked files of the test repo say where
// the specs are: that is what makes `tests/`, or any directory above the specs, select them.
func (ex *execution) newFileFilter(files []string, all []listedTest) fileFilter {
	repo := ex.suite.Test.Repo
	dir := ex.dirs[repo]
	roots := []string{dir}
	for _, h := range ex.j.req.Heads {
		if h.Repo == repo {
			roots = append(roots, h.Dir)
		}
	}
	var out fileFilter
	for _, f := range files {
		f = filepath.Clean(f)
		for _, root := range roots {
			if root == "" || !filepath.IsAbs(f) {
				continue
			}
			if rel, err := filepath.Rel(root, f); err == nil && filepath.IsLocal(rel) {
				f = rel
				break
			}
		}
		out.typed = append(out.typed, strings.TrimSuffix(filepath.ToSlash(f), "/"))
	}
	tracked, err := gitOut(ex.ctx, dir, "ls-files", "-z")
	if err != nil {
		// The names alone still match; a directory that only the repo could resolve then selects nothing, which
		// fails the run.
		ex.emit(0, "run.filter", "", "", map[string]string{"error": trim(err.Error(), 300)})
		return out
	}
	names := make([]string, len(all))
	for i, t := range all {
		names[i] = t.file
	}
	out.paths = specPaths(strings.Split(tracked, "\x00"), names)
	return out
}

// specPaths finds where each listed file is in the repository. The listed names share a base, the test directory:
// a directory under which every name that is tracked anywhere exists. It places every listed name, a spec generated
// before the run (not tracked) included. When no directory holds them all (specs from two places), only a tracked
// name that is in one place is placed.
func specPaths(tracked, listed []string) map[string][]string {
	byBase := map[string][]string{}
	for _, p := range tracked {
		byBase[path.Base(p)] = append(byBase[path.Base(p)], p)
	}
	under := map[string][]string{} // listed file -> the directories it could be relative to ("" is the root)
	var common []string
	first := true
	for _, file := range listed {
		if _, done := under[file]; done || !filepath.IsLocal(file) {
			continue
		}
		var dirs []string
		for _, p := range byBase[path.Base(file)] {
			if p == file {
				dirs = append(dirs, "")
			} else if d, ok := strings.CutSuffix(p, "/"+file); ok {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) == 0 {
			continue
		}
		under[file] = dirs
		if first {
			common, first = slices.Clone(dirs), false
		} else {
			common = slices.DeleteFunc(common, func(d string) bool { return !slices.Contains(dirs, d) })
		}
	}
	out := map[string][]string{}
	for _, file := range listed {
		dirs := common
		if len(dirs) == 0 && len(under[file]) == 1 {
			dirs = under[file]
		}
		if out[file] != nil {
			continue
		}
		for _, d := range dirs {
			out[file] = append(out[file], path.Join(d, file))
		}
	}
	return out
}

// matches says whether a listed file is one of the files asked for or inside one of the directories. People type paths
// from the repository root or as absolute paths while the list is relative to the test directory, so a filter matches
// when its trailing path parts name the listed file or a directory above it, or when the file's place in the
// repository is inside it.
func (ff fileFilter) matches(file string) bool {
	for _, f := range ff.typed {
		// The root of the repository holds every spec, tracked or not.
		if f == "." || strings.HasSuffix(file, "/"+f) {
			return true
		}
		for tail := f; tail != ""; {
			if file == tail || strings.HasPrefix(file, tail+"/") {
				return true
			}
			_, rest, ok := strings.Cut(tail, "/")
			if !ok {
				break
			}
			tail = rest
		}
		for _, p := range ff.paths[file] {
			if p == f || strings.HasPrefix(p, f+"/") {
				return true
			}
		}
	}
	return false
}

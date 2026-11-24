package suite

import "context"

// onDefaultBranch says whether every repository of the run was tested at a commit the mirror's default branch
// contains: what such a run learns is evidence about the product as it is on main, so it may replace what is
// stored; any other run (a pull request, a branch) may only add to it. It is the same rule coverage follows in
// package affected, which cannot be imported here.
func onDefaultBranch(ctx context.Context, repos map[string]RepoState) bool {
	if len(repos) == 0 {
		return false
	}
	for _, st := range repos {
		if st.Mirror == nil || st.Head == "" {
			return false
		}
		if _, err := gitOut(ctx, st.Mirror.Dir, "merge-base", "--is-ancestor", st.Head, "HEAD"); err != nil {
			return false
		}
	}
	return true
}

// replaces says whether what this run learns about write sets replaces the stored sets: never in a fork, and only
// on every repository's default branch otherwise. Decided once, the heads do not change during a run.
func (ex *execution) replaces() bool {
	ex.wr.once.Do(func() {
		ex.wr.replace = !ex.fork() && onDefaultBranch(ex.bg, ex.repoStates())
	})
	return ex.wr.replace
}

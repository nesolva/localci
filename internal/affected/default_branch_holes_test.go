package affected

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/suite"
)

// holeRig learns an entry for t1 on the default branch, then lets a test say what a later default-branch run
// understood only in part. Whatever the later run could not fully understand must not leave the older complete
// entry in place, or a change in the part that is unknown would skip the test.
type holeRig struct {
	t    *testing.T
	p    *Planner
	dir  string
	head string
}

func (r *holeRig) done(in suite.DoneInput) {
	in.Run, in.Suite = "1", testSuite()
	in.Repos = map[string]suite.RepoState{shop: {Head: r.head, Dir: r.dir, Mirror: &exec.Mirror{Dir: r.dir}}}
	in.Passed, in.Ran = []string{"t1"}, []suite.TestRef{{ID: "t1"}}
	r.p.Done(context.Background(), in)
}

func TestAPartlyUnderstoodDefaultBranchRunDropsTheOlderCompleteEntry(t *testing.T) {
	tc := suite.TestCtx{Run: "1", Test: "t1", Suite: "e2e"}
	long := strings.Repeat("d/", 300) + "f.js"
	many := make([]string, 5001)
	for i := range many {
		many[i] = fmt.Sprintf("%s:src/f%d.js", shop, i)
	}
	cases := []struct {
		name     string
		backends bool
		run      func(r *holeRig)
	}{
		{"incomplete with no files, no backend", false, func(r *holeRig) {
			r.done(suite.DoneInput{Frontend: map[string][]string{"t1": {}}, Incomplete: map[string]bool{"t1": true}})
		}},
		{"incomplete with no files, complete backend", true, func(r *holeRig) {
			r.p.mark(tc, []string{shop + ":src/orders.js"}, false, false)
			r.done(suite.DoneInput{Frontend: map[string][]string{"t1": {}}, Incomplete: map[string]bool{"t1": true}})
		}},
		{"the backend measurement failed", true, func(r *holeRig) {
			r.p.mark(tc, nil, true, false)
			r.done(suite.DoneInput{})
		}},
		{"the backend measured nothing for the test", true, func(r *holeRig) {
			r.done(suite.DoneInput{})
		}},
		{"more files than a choice can use", true, func(r *holeRig) {
			r.p.mark(tc, many, false, false)
			r.done(suite.DoneInput{})
		}},
		{"a path too long to keep", true, func(r *holeRig) {
			r.p.mark(tc, []string{shop + ":src/orders.js", shop + ":" + long}, false, false)
			r.done(suite.DoneInput{})
		}},
	}
	for _, tcase := range cases {
		t.Run(tcase.name, func(t *testing.T) {
			p, st := newPlanner(t)
			if tcase.backends {
				p.cfg["e2e"] = Config{Backends: []Backend{{Stack: "api", Repo: shop}}}
			}
			dir := t.TempDir()
			gitIn(t, dir, "init", "-q", "-b", "main")
			head := commitFile(t, dir, "src/orders.js")
			r := &holeRig{t: t, p: p, dir: dir, head: head}
			if tcase.backends {
				p.mark(tc, []string{shop + ":src/orders.js"}, false, false)
				r.done(suite.DoneInput{})
			} else {
				r.done(suite.DoneInput{Frontend: map[string][]string{"t1": {"src/orders.js"}}})
			}
			if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":src/orders.js"}) {
				t.Fatalf("setup: %v", got)
			}
			tcase.run(r)
			if got := filesIn(t, st, "t1"); got != nil {
				t.Fatalf("the older complete entry survived a run that understood the test only in part: %v", got)
			}
		})
	}
}

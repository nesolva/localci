package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/nesolva/localci/internal/exec"
)

// AffectedSuffix ends the status name of an affected run: localci/<suite> (affected). A suite name never holds a
// space or a slash, so the name cannot collide with a suite's or a check's context.
const AffectedSuffix = " (affected)"

// StatusName is the name a run posts its status under (forge.Context adds the localci/ prefix).
func StatusName(suite string, affected bool) string {
	if affected {
		return suite + AffectedSuffix
	}
	return suite
}

// AffectedRequest asks for only the tests the diff can affect.
type AffectedRequest struct {
	// Base is the branch the diff starts from (the merge base with it); empty means the default branch.
	Base string
	// Explain stops the run after the choice: it runs no test, and the run.affected event says why.
	Explain bool
}

// Affector chooses the tests of an affected run and learns from every run; internal/affected implements it.
type Affector interface {
	// Plan picks the tests. An error runs every test: a choice that cannot be made is not safe.
	Plan(ctx context.Context, in PlanInput) (Plan, error)
	// Done is told how a run ended, to refresh what it learned. It is not called for runs that did not finish.
	Done(ctx context.Context, in DoneInput)
}

// RepoState is one repository of a run: its mirror, the commit tested and where it is checked out.
type RepoState struct {
	Mirror *exec.Mirror
	Head   string
	Dir    string
}

// TestRef is a listed test.
type TestRef struct{ ID, File, Title string }

// PlanInput is what a choice needs.
type PlanInput struct {
	Run     string
	Suite   *Suite
	Request AffectedRequest
	Repos   map[string]RepoState
	Tests   []TestRef // every listed test, before any filter
}

// Plan is the choice. Detail becomes the data of the run.affected event.
type Plan struct {
	All    bool
	IDs    []string
	Detail json.RawMessage
}

// DoneInput describes a finished run.
type DoneInput struct {
	Run   string
	Suite *Suite
	// Fork: the report of the run is not trusted to teach anything.
	Fork   bool
	Repos  map[string]RepoState
	Passed []string // tests that passed
	// Listed holds every listed test when the run ran all of them; nil when the run was narrowed.
	Listed []TestRef
	Ran    []TestRef
	// Frontend is the coverage events the package reported, test id → files as it wrote them.
	Frontend map[string][]string
	// Incomplete holds the tests whose coverage event said some source could not be attributed.
	Incomplete map[string]bool
}

// SetAffector installs what chooses tests and learns coverage. Call it before runs start.
func (r *Runner) SetAffector(a Affector) {
	r.mu.Lock()
	r.affector = a
	r.mu.Unlock()
}

// withBase adds the branch the diff started from to the event's data, so a retry of the run can ask for the
// same diff.
func withBase(detail json.RawMessage, base string) json.RawMessage {
	if base == "" {
		return detail
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(detail, &m) != nil {
		return detail
	}
	b, _ := json.Marshal(base)
	m["base"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return detail
	}
	return out
}

// selectAffected narrows tests (the filtered list) to what the plan chooses from all of the listed tests.
func (ex *execution) selectAffected(all, tests []listedTest) ([]listedTest, error) {
	ex.r.mu.Lock()
	a := ex.r.affector
	ex.r.mu.Unlock()
	req := *ex.j.req.Affected
	refs := make([]TestRef, len(all))
	for i, t := range all {
		refs[i] = TestRef{t.id, t.file, t.title}
	}
	plan, err := a.Plan(ex.ctx, PlanInput{Run: ex.id, Suite: ex.suite, Request: req, Repos: ex.repoStates(), Tests: refs})
	if err != nil {
		if ex.ctx.Err() != nil {
			return nil, ex.ctx.Err()
		}
		// Not being able to choose is not a reason to skip tests.
		detail, _ := json.Marshal(map[string]any{"all": true, "reason": "could not choose: " + trim(err.Error(), 300),
			"total": len(all), "changed": []string{}})
		plan = Plan{All: true, Detail: detail}
	}
	ex.emit(0, "run.affected", "", "", withBase(plan.Detail, req.Base))
	if req.Explain {
		ex.explainOnly = true
		return nil, nil
	}
	if plan.All {
		return tests, nil
	}
	var out []listedTest
	for _, t := range tests {
		if slices.Contains(plan.IDs, t.id) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (ex *execution) repoStates() map[string]RepoState {
	out := map[string]RepoState{}
	ex.mu.Lock()
	defer ex.mu.Unlock()
	for repo, dir := range ex.dirs {
		out[repo] = RepoState{Mirror: ex.mirrors[repo], Head: ex.shas[repo], Dir: dir}
	}
	return out
}

// affectedDone hands a finished run to the affector.
func (ex *execution) affectedDone(all, ran []listedTest) {
	ex.r.mu.Lock()
	a := ex.r.affector
	ex.r.mu.Unlock()
	if a == nil || ex.explainOnly {
		return
	}
	refs := func(l []listedTest) []TestRef {
		out := make([]TestRef, len(l))
		for i, t := range l {
			out[i] = TestRef{t.id, t.file, t.title}
		}
		return out
	}
	in := DoneInput{Run: ex.id, Suite: ex.suite, Fork: !ex.learns(), Repos: ex.repoStates(), Ran: refs(ran)}
	if len(ran) == len(all) {
		in.Listed = refs(all)
	}
	last := map[string]attempt{}
	ex.mu.Lock()
	for _, at := range ex.results {
		if p, ok := last[at.id]; !ok || at.row.Retry >= p.row.Retry {
			last[at.id] = at
		}
	}
	ex.mu.Unlock()
	for id, at := range last {
		if at.passed() {
			in.Passed = append(in.Passed, id)
		}
	}
	slices.Sort(in.Passed)
	in.Frontend, in.Incomplete = map[string][]string{}, map[string]bool{}
	evs, err := ex.r.d.Store.RunEvents(ex.bg, ex.id)
	if err != nil {
		return
	}
	for _, e := range evs {
		if e.Kind != "coverage" {
			continue
		}
		var c struct {
			Test       string   `json:"test"`
			Files      []string `json:"files"`
			Incomplete bool     `json:"incomplete"`
		}
		if json.Unmarshal(e.Data, &c) == nil && c.Test != "" {
			in.Frontend[c.Test] = append(in.Frontend[c.Test], c.Files...)
			in.Incomplete[c.Test] = in.Incomplete[c.Test] || c.Incomplete
		}
	}
	a.Done(ex.bg, in)
}

func (r *Runner) checkAffected(req Request) error {
	if req.Affected == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.affector == nil {
		return fmt.Errorf("affected runs are not available here")
	}
	return nil
}

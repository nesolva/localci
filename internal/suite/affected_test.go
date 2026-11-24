package suite

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

// stubAffector picks the ids it is given and records what runs tell it.
type stubAffector struct {
	mu   sync.Mutex
	ids  []string
	all  bool
	err  error
	seen []PlanInput
	done []DoneInput
}

func (a *stubAffector) Plan(_ context.Context, in PlanInput) (Plan, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, in)
	detail, _ := json.Marshal(map[string]any{"all": a.all, "total": len(in.Tests)})
	return Plan{All: a.all, IDs: a.ids, Detail: detail}, a.err
}

func (a *stubAffector) Done(_ context.Context, in DoneInput) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = append(a.done, in)
}

func tested(g *rig, run string) []string {
	var ids []string
	for _, a := range g.attempts(run) {
		if a.Status != "running" {
			ids = append(ids, a.Test)
		}
	}
	return ids
}

func TestAffectedRunRunsOnlyTheChosenTests(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b", "c"}, 4)
	st := &stubAffector{ids: []string{"b"}}
	g.r.SetAffector(st)

	run := g.run(Request{Suite: "e2e", Heads: mainHead(), Affected: &AffectedRequest{}})
	if run.State != store.StatePassed || !run.Affected {
		t.Fatalf("run = %+v, want passed and affected", run)
	}
	if got := tested(g, run.ID); len(got) != 1 || got[0] != "b" {
		t.Fatalf("tests run = %v, want only b", got)
	}
	if len(st.seen) != 1 || len(st.seen[0].Tests) != 3 {
		t.Fatalf("the choice must see every listed test, got %+v", st.seen)
	}
	if ev, ok, _ := g.st.FirstEvent(context.Background(), run.ID, "run.affected"); !ok || len(ev.Data) == 0 {
		t.Fatal("the choice is not in the run's events: --explain and the dashboard could not say why")
	}
	// The run was narrowed: it must not look like a full one, or the map would forget the tests it skipped.
	if len(st.done) != 1 || st.done[0].Listed != nil || len(st.done[0].Ran) != 1 {
		t.Fatalf("done = %+v, want one narrowed run", st.done)
	}
}

func TestAffectedChoiceThatCannotBeMadeRunsEverything(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	g.r.SetAffector(&stubAffector{err: context.DeadlineExceeded})
	run := g.run(Request{Suite: "e2e", Heads: mainHead(), Affected: &AffectedRequest{}})
	if got := tested(g, run.ID); len(got) != 2 {
		t.Fatalf("tests run = %v, want both: not being able to choose is not a reason to skip tests", got)
	}
}

func TestAffectedExplainRunsNoTest(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	st := &stubAffector{ids: []string{"a"}}
	g.r.SetAffector(st)
	run := g.run(Request{Suite: "e2e", Heads: mainHead(), Affected: &AffectedRequest{Explain: true}})
	if run.State != store.StatePassed || !strings.Contains(run.Reason, "explain only") {
		t.Fatalf("run = %s %q", run.State, run.Reason)
	}
	if got := tested(g, run.ID); len(got) != 0 {
		t.Fatalf("an explain run ran %v", got)
	}
	if len(st.done) != 0 {
		t.Fatal("an explain run measured nothing and must not touch the coverage map")
	}
}

func TestAffectedNothingAffectedPassesWithoutRunning(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 4)
	g.r.SetAffector(&stubAffector{})
	run := g.run(Request{Suite: "e2e", Heads: mainHead(), Affected: &AffectedRequest{}})
	if run.State != store.StatePassed || run.Reason != "no tests affected" {
		t.Fatalf("run = %s %q", run.State, run.Reason)
	}
}

func TestFullRunTellsTheMapEveryTestAndTheForkFlag(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	st := &stubAffector{}
	g.r.SetAffector(st)
	run := g.run(Request{Suite: "e2e", Heads: mainHead(), Fork: true})
	if !run.Fork || run.Affected {
		t.Fatalf("run = %+v, want a fork run that is not affected", run)
	}
	d := st.done[0]
	if !d.Fork || len(d.Listed) != 2 || len(d.Passed) != 2 {
		t.Fatalf("done = %+v, want the fork flag, both tests listed and both passed", d)
	}
}

func TestAffectedWithoutAnAffectorIsRefused(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 4)
	if _, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead(), Affected: &AffectedRequest{}}); err == nil {
		t.Fatal("an affected run with nothing to choose would run everything under the affected status")
	}
}

// A full run of a pull request must not cancel its affected run: their statuses differ, and the cancelled one
// would stay pending on the pull request.
func TestFullAndAffectedRunsOfOnePullRequestDoNotSupersedeEachOther(t *testing.T) {
	full := Request{Heads: prHead(7)}
	aff := Request{Heads: prHead(7), Affected: &AffectedRequest{}}
	if supersedeKey(full) == supersedeKey(aff) {
		t.Fatal("same key: the label run would cancel the push run")
	}
	if supersedeKey(aff) != supersedeKey(Request{Heads: prHead(7), Affected: &AffectedRequest{Base: "dev"}}) {
		t.Fatal("two affected runs of one pull request must supersede each other")
	}
	if supersedeKey(Request{Heads: mainHead(), Affected: &AffectedRequest{}}) != "" {
		t.Fatal("a run of no pull request is never superseded")
	}
}

// A run whose stack broke hands the coverage map to nobody as trusted: what its passed tests reached was measured
// beside a broken stack, so the affector is told it is not a run to learn from.
func TestInfraRunIsNotALearningRunForTheCoverageMap(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 4)
	st := &stubAffector{}
	g.r.SetAffector(st)
	ctx := context.Background()
	ex := &execution{r: g.r, j: &job{}, suite: g.r.suites["e2e"], id: "3", ctx: ctx, bg: ctx}
	ex.affectedDone(nil, nil)
	ex.setInfra("stack app exited")
	ex.affectedDone(nil, nil)
	if len(st.done) != 2 || st.done[0].Fork || !st.done[1].Fork {
		t.Fatalf("done = %+v, want the second call flagged as not to learn from", st.done)
	}
}

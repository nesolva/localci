package suite

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/triage"
)

// triageConfig is the suite's `triage` key; a bad value is reported once and the defaults stand.
func (ex *execution) triageConfig() triage.Config {
	c, err := triage.ParseConfig(ex.suite.Raw)
	if err != nil {
		ex.emit(0, "triage.config", "", "", map[string]string{"error": err.Error()})
	}
	return c
}

// extraReruns runs the tests that still fail after the runner's own reruns alone again, up to the suite's
// `triage.reruns` times, to rule out flakiness: a pass anywhere makes the test flaky. It runs inside the run, so
// it is past the gate that let the run start (a fork's pull request included). Nothing is added unless asked.
func (ex *execution) extraReruns(e *shardEnv, failed []shard.TestID) error {
	n := ex.triageConfig().Reruns
	for i := 1; i <= n; i++ {
		var again []shard.TestID
		last := ex.lastRerun()
		for _, id := range failed {
			if a, ok := last[string(id)]; ok && a.failed() && a.row.Status != "interrupted" {
				again = append(again, id)
			}
		}
		if len(again) == 0 {
			return nil
		}
		inv, err := e.invoke(ex.ctx, again, modeRerun, fmt.Sprintf(".rerun%d", i+2))
		if err != nil || inv.crashed {
			return err
		}
	}
	return nil
}

// lastRerun is the newest rerun attempt of every test that was rerun.
func (ex *execution) lastRerun() map[string]attempt {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	last := map[string]attempt{}
	for _, a := range ex.results {
		if a.mode == modeRerun {
			last[a.id] = a
		}
	}
	return last
}

// triage classifies what failed in the run (and what failed on the way and still passed) and stores class and
// evidence on the tests. It never fails the run: a triage that cannot work leaves the tests unclassified.
func (ex *execution) triage() {
	if ex.j.ctx.Err() != nil {
		return
	}
	st := ex.r.d.Store
	ex.mu.Lock()
	results := slices.Clone(ex.results)
	listedTests := make(map[string]listed, len(ex.tests))
	for k, v := range ex.tests {
		listedTests[k] = v
	}
	ex.mu.Unlock()

	attempts := map[string][]triage.Attempt{}
	var order []string
	anyFailed := false
	for _, a := range results {
		at := triage.Attempt{Status: a.row.Status, Rerun: a.mode == modeRerun}
		if len(a.row.Failure) > 0 {
			var f report.Failure
			if json.Unmarshal(a.row.Failure, &f) == nil {
				at.Failure = &f
			}
		}
		if _, seen := attempts[a.id]; !seen {
			order = append(order, a.id)
		}
		attempts[a.id] = append(attempts[a.id], at)
		anyFailed = anyFailed || at.Failed()
	}
	if !anyFailed {
		return
	}

	in := triage.RunInput{Run: ex.id, Tests: order, Attempts: attempts, Moved: map[string]bool{}, LogDir: ex.logDir}
	in.Health.Unreadable = int(ex.badLines.Load())
	if evs, err := st.RunEvents(ex.bg, ex.id); err == nil {
		for _, e := range evs {
			switch e.Kind {
			case "report.invalid":
				in.Health.Invalid++
			case "test.serial":
				in.Moved[e.Test] = true
			}
		}
	}
	in.Steps, _ = st.StepCounts(ex.bg, ex.id)
	for _, dir := range ex.dirs {
		if out, err := gitOut(ex.bg, dir, "diff", "--name-only", "HEAD^1", "HEAD"); err == nil {
			in.Changed = append(in.Changed, strings.Fields(out)...)
		}
	}
	in.Workspace = ex.triageWorkspace(listedTests)

	res, err := triage.Run(ex.bg, st, in)
	if err != nil {
		ex.emit(0, "triage.error", "", "", map[string]string{"error": trim(err.Error(), 300)})
		return
	}
	ex.mu.Lock()
	ex.flaky = res.Flaky
	ex.mu.Unlock()
	ex.emit(0, "triage.done", "", "", map[string]int{"classified": res.Classified, "flaky": res.Flaky})
}

// triageWorkspace says where the test files are and which commits a patch to them fits: the tested commit and,
// when the run tested a working tree, the commit that tree was made on.
func (ex *execution) triageWorkspace(listedTests map[string]listed) triage.Workspace {
	repo := ex.suite.Test.Repo
	w := triage.Workspace{Dir: ex.dirs[repo]}
	seen := map[string]bool{}
	for _, t := range listedTests {
		if t.File != "" && !seen[t.File] {
			seen[t.File] = true
			w.Files = append(w.Files, t.File)
		}
	}
	slices.Sort(w.Files)
	if w.Dir == "" {
		return w
	}
	if sha, err := gitOut(ex.bg, w.Dir, "rev-parse", "HEAD"); err == nil {
		w.Shas = append(w.Shas, sha)
	}
	for _, h := range ex.j.req.Heads {
		if h.Repo == repo && h.Dir != "" && h.SHA != "" && !slices.Contains(w.Shas, h.SHA) {
			w.Shas = append(w.Shas, h.SHA)
		}
	}
	return w
}

// flakyNote is the part of a run's reason that says how many tests failed on the way and passed in the end.
func (ex *execution) flakyNote() string {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.flaky == 0 {
		return ""
	}
	return fmt.Sprintf("%d flaky (failed, then passed): localci triage %s", ex.flaky, ex.id)
}

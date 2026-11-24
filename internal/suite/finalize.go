package suite

import (
	"encoding/json"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// MovedNote is the note a conflicting test carries when it passes alone.
const MovedNote = "moved to serial group"

// finalize finds the tests that only fail next to others, puts them in the serial group, and records what
// this run taught about durations.
func (ex *execution) finalize() error {
	if !ex.learns() {
		// A fork's code and report are not reviewed, and a broken stack or tenant flow says nothing about the
		// tests: neither teaches later runs anything (serial group, suspects, durations). A fork's reruns
		// still decide its own outcome.
		return nil
	}
	ex.saveMemo()
	ex.mu.Lock()
	var parallel, alone []shard.Result
	lastAlone := map[string]attempt{}
	for _, a := range ex.results {
		r := shard.Result{ID: shard.TestID(a.id), Shard: a.lane, Passed: a.passed(), Start: a.start, End: a.end}
		switch a.mode {
		case modeParallel:
			parallel = append(parallel, r)
		case modeRerun:
			alone = append(alone, r)
			lastAlone[a.id] = a
		}
	}
	ex.mu.Unlock()

	st := ex.r.d.Store
	for _, c := range shard.Detect(parallel, alone) {
		test := string(c.ID)
		with := make([]string, len(c.Evidence))
		for i, e := range c.Evidence {
			with[i] = string(e)
		}
		evidence, _ := json.Marshal(map[string]any{"run": ex.id, "failed_next_to": with})
		if err := st.AddSerial(ex.bg, store.SerialEntry{Suite: ex.suite.Name, Test: test, Evidence: evidence}); err != nil {
			return err
		}
		// The rerun that passed is the test's last word: it passed, and says why it moved.
		if a, ok := lastAlone[test]; ok && a.passed() {
			note, _ := json.Marshal(map[string]any{"note": MovedNote, "failed_next_to": with})
			a.row.Evidence = note
			if err := st.PutTest(ex.bg, a.row); err != nil {
				return err
			}
		}
		ex.mu.Lock()
		ex.moved++
		ex.mu.Unlock()
		ex.emit(0, "test.serial", test, "", map[string]any{"failed_next_to": with})
	}

	if err := ex.confirmSuspects(); err != nil {
		return err
	}

	ex.mu.Lock()
	results := append([]attempt(nil), ex.results...)
	ex.mu.Unlock()
	for _, a := range results {
		if a.mode != modeRerun && a.passed() && a.row.MS > 0 {
			if err := st.RecordDuration(ex.bg, a.id, a.row.MS); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ex *execution) movedCount() int {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.moved
}

// fork says the run's heads include a pull request head from another repository.
func (ex *execution) fork() bool { return ex.j != nil && ex.j.req.Fork }

// learns says whether what this run saw may teach later runs: durations, the serial group, suspects, write sets,
// coverage, the flaky list. A fork's report is not trusted, and a run whose stack or tenant flow broke measured
// the breakage, not the tests. What was stored from tests that passed before the run turned infrastructure stays;
// nothing is stored after.
func (ex *execution) learns() bool { return !ex.fork() && ex.infraReason() == "" }

// infraReason is why the run is an infrastructure failure, "" when it is not (yet).
func (ex *execution) infraReason() string {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.infra
}

// setInfra marks the run an infrastructure failure: the stack or the tenant flow broke, the tests did not. The
// first reason stays. It is stored on the run before the run ends, so the status posted at the end finds it.
func (ex *execution) setInfra(reason string) {
	// Often text of the code under test (a stack's last words): it reaches the forge's status, so no URL query or
	// secret goes through.
	reason = report.Reason(report.Scrub(reason))
	if reason == "" {
		reason = "unknown cause"
	}
	ex.mu.Lock()
	first := ex.infra == ""
	if first {
		ex.infra = reason
	}
	ex.mu.Unlock()
	if !first {
		return
	}
	if _, err := ex.r.d.Store.SetRunInfra(ex.bg, ex.id, reason); err != nil {
		ex.emit(0, "run.infra.failed", "", "", map[string]string{"error": trim(err.Error(), 300)})
	}
	ex.emit(0, "run.infra", "", "", map[string]string{"reason": reason})
}

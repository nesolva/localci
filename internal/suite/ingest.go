package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// mode says what an invocation of the test command is for.
type mode int

const (
	modeParallel mode = iota // a shard of the parallel part
	modeSerial               // the serial group
	modeRerun                // a failed test run again, alone
	modeProbe                // a test run alone only to measure what it writes; never a result
)

// attempt is one finished execution of one test in this run.
type attempt struct {
	id    string
	lane  int
	mode  mode
	start time.Time
	end   time.Time
	row   store.Test
}

func (a attempt) passed() bool { return a.row.Status == "passed" }

func (a attempt) failed() bool {
	switch a.row.Status {
	case "failed", "timedOut", "interrupted":
		return true
	}
	return false
}

// ingest turns the report events of one invocation into store rows and log events.
type ingest struct {
	ex       *execution
	n        int
	mode     mode
	cur      map[string]*begun
	reported map[string]bool
	ended    bool

	badOnce sync.Once
	// versionErr is set when the report says it speaks another protocol version; read after the tail is done.
	versionErr error
}

type begun struct {
	attempt int
	file    string
	title   string
	line    int
	ts      int64
}

func newIngest(ex *execution, n int, m mode) *ingest {
	return &ingest{ex: ex, n: n, mode: m, cur: map[string]*begun{}, reported: map[string]bool{}}
}

// follow tails the report file; stop ends it after one last read, and the returned channel closes when
// everything written has been handled.
func (i *ingest) follow(path string) (stop func(), done <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan struct{})
	t := &report.Tailer{Path: path, Interval: 25 * time.Millisecond, OnEvent: i.handle, OnBad: i.bad}
	go func() { defer close(ch); t.Run(ctx) }()
	return cancel, ch
}

// bad counts a report line nobody could read; the run says how many at the end, and the first one is logged.
func (i *ingest) bad(line []byte, err error) {
	i.ex.badLines.Add(1)
	i.badOnce.Do(func() {
		i.ex.emit(i.n, "report.unreadable", "", "", map[string]string{"error": trim(err.Error(), 300), "line": trim(string(line), 300)})
	})
}

func (ex *execution) nextAttempt(test string) int {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	n := ex.attempts[test]
	ex.attempts[test] = n + 1
	return n
}

// laneFor is the shard lane a test's rows belong to: where it first ran.
func (i *ingest) laneFor(test string) int {
	ex := i.ex
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if l, ok := ex.laneOf[test]; ok && i.mode == modeRerun {
		return l
	}
	l := i.n
	if i.mode == modeSerial {
		l = 0
	}
	if _, ok := ex.laneOf[test]; !ok {
		ex.laneOf[test] = l
	}
	return l
}

func ms(v float64) int64 { return int64(math.Round(v)) }

func tsOf(ev report.Event) time.Time {
	if ev.TS > 0 {
		return time.UnixMilli(ev.TS)
	}
	return time.Now()
}

func (i *ingest) handle(ev report.Event) {
	ex, st := i.ex, i.ex.r.d.Store
	bg := ex.bg
	if ev.V != 1 {
		i.bad(ev.Raw, fmt.Errorf("report version %d is not supported (this localci reads version 1)", ev.V))
		if ev.Type == "begin" && i.versionErr == nil {
			i.versionErr = fmt.Errorf("shard %d: the report is version %d, this localci reads version 1: upgrade the framework package or localci", i.n, ev.V)
		}
	}
	if i.mode == modeProbe { // measured by the hooks; its events and results are not the test's
		if _, ok := ev.Body.(*report.End); ok {
			i.ended = true
		}
		return
	}
	var test, step string
	switch b := ev.Body.(type) {
	case *report.TestBegin:
		test = b.Test
		n := ex.nextAttempt(b.Test)
		if rec, _ := ex.memoState(); rec != nil {
			rec.Begin(b.Test)
		}
		i.cur[b.Test] = &begun{attempt: n, file: b.File, title: b.Title, line: b.Line, ts: ev.TS}
		st.PutTest(bg, store.Test{Run: ex.id, Shard: i.laneFor(b.Test), ID: b.Test, File: b.File, Title: b.Title,
			Line: b.Line, Status: "running", Retry: n})
	case *report.StepBegin:
		test, step = b.Test, b.Step
		if rec, _ := ex.memoState(); rec != nil {
			rec.Step(b.Test, b.Kind)
		}
		if i.mode != modeRerun {
			parent := ""
			if b.Parent != nil {
				parent = *b.Parent
			}
			st.PutStep(bg, store.Step{Run: ex.id, Test: b.Test, ID: b.Step, Parent: parent, Title: b.Title, Kind: b.Kind, Start: tsOf(ev).UnixMilli()})
		}
	case *report.StepEnd:
		step = b.Step
		if k := strings.LastIndex(b.Step, ":"); k > 0 {
			test = b.Step[:k]
			if i.mode != modeRerun {
				st.EndStep(bg, ex.id, test, b.Step, ms(b.MS), b.OK)
			}
		}
	case *report.Artifact:
		test = b.Test
		st.PutArtifact(bg, store.Artifact{Run: ex.id, Test: b.Test, Kind: b.Kind, Path: b.Path})
	case *report.TestEnd:
		test = b.Test
		i.testEnd(ev, b)
	case *report.FlowBegin:
		test, step = b.Test, b.Step
		i.tenantFlowBegin(b)
		b.Sanitize()
		if rec, _ := ex.memoState(); rec != nil {
			rec.FlowBegin(b.Test, b.Step, b.Flow, b.Inputs)
		}
		ev.Raw = report.RedactedLine(ev.Raw, b)
	case *report.FlowEnd:
		step = b.Step
		if k := strings.LastIndex(b.Step, ":"); k > 0 {
			test = b.Step[:k]
		}
		i.tenantFlowEnd(b)
		if rec, plan := ex.memoState(); rec != nil {
			if idx, sig, top := rec.FlowEnd(test, b.Step, b.Checkpoint); top && plan != nil {
				plan.FlowEnd(test, idx, sig)
			}
		}
		b.Sanitize()
		ev.Raw = report.RedactedLine(ev.Raw, b)
	case *report.Writes:
		test = b.Test
		i.reportedWrites(b)
	case *report.End:
		i.ended = true
	}
	// The type is the report's own text and becomes an event kind: anything but a plain word, or a word that would
	// pass for one of localci's own events, keeps its raw line as data under a fixed kind.
	kind := ev.Type
	if !store.ValidKind(kind) || (!report.IsType(kind) && coreKind(kind)) {
		kind = "report.invalid"
	}
	ex.emit(i.n, kind, test, step, ev.Raw)
}

// coreKind reports whether kind belongs to the events localci itself emits (run, shard, step, test and database
// state, checkout, prepare), which followers trust.
func coreKind(kind string) bool {
	switch kind {
	case "checkout", "prepare", "run", "shard", "step", "test", "db", "checkpoint":
		return true
	}
	for _, p := range []string{"run.", "shard.", "step.", "test.", "db.", "checkpoint."} {
		if strings.HasPrefix(kind, p) {
			return true
		}
	}
	return false
}

func (i *ingest) testEnd(ev report.Event, b *report.TestEnd) {
	ex := i.ex
	c := i.cur[b.Test]
	if c == nil {
		c = &begun{attempt: ex.nextAttempt(b.Test), ts: ev.TS}
	}
	delete(i.cur, b.Test)
	status := b.Status
	if status == "timedout" {
		status = "timedOut" // the spelling the store's failure query knows
	}
	row := store.Test{Run: ex.id, Shard: i.laneFor(b.Test), ID: b.Test, File: c.file, Title: c.title, Line: c.line,
		Status: status, MS: ms(b.MS), Retry: c.attempt}
	if ex.tests != nil {
		if t, ok := ex.tests[b.Test]; ok && row.File == "" {
			row.File, row.Title, row.Line = t.File, t.Title, t.Line
		}
	}
	if b.Failure != nil {
		b.Failure.Sanitize()
		row.Failure, _ = json.Marshal(b.Failure)
	}
	ex.r.d.Store.PutTest(ex.bg, row)
	i.reported[b.Test] = true
	ex.testOutcome(b.Test, status)
	if rec, _ := ex.memoState(); rec != nil {
		rec.End(b.Test, status == "passed" || status == "flaky")
	}
	start := time.UnixMilli(c.ts)
	if c.ts == 0 {
		start = tsOf(ev).Add(-time.Duration(b.MS) * time.Millisecond)
	}
	ex.mu.Lock()
	ex.results = append(ex.results, attempt{id: b.Test, lane: row.Shard, mode: i.mode, start: start, end: tsOf(ev), row: row})
	ex.mu.Unlock()
}

// finish records the tests of ids the command never reported, and says whether it ended without finishing its
// report.
func (i *ingest) finish(res exec.Result, ids []shard.TestID) (crashed bool) {
	crashed = !i.ended
	ex := i.ex
	if i.mode == modeProbe {
		return crashed
	}
	why := "the test command did not report this test"
	if crashed {
		why = "the test command ended before this test finished"
		if res.ExitCode != 0 {
			why = fmt.Sprintf("the test command exited with code %d before this test finished", res.ExitCode)
		}
	}
	for _, id := range ids {
		test := string(id)
		if i.reported[test] {
			continue
		}
		failure, _ := json.Marshal(map[string]any{"step": "", "message": why})
		row := store.Test{Run: ex.id, Shard: i.laneFor(test), ID: test, Status: "interrupted", Failure: failure,
			Retry: ex.nextAttempt(test)}
		ex.mu.Lock()
		if t, ok := ex.tests[test]; ok {
			row.File, row.Title, row.Line = t.File, t.Title, t.Line
		}
		ex.results = append(ex.results, attempt{id: test, lane: row.Shard, mode: i.mode, start: time.Now(), end: time.Now(), row: row})
		ex.mu.Unlock()
		ex.r.d.Store.PutTest(ex.bg, row)
	}
	return crashed
}

// laneFailed reports whether any test of the parallel shard n has a failing last attempt.
func (ex *execution) laneFailed(n int) bool {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	for _, a := range ex.results {
		if a.mode == modeParallel && a.lane == n && a.failed() {
			return true
		}
	}
	return false
}

// failedParallel lists the tests that failed in the parallel part, in the order they finished.
func (ex *execution) failedParallel() []shard.TestID {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	var out []shard.TestID
	seen := map[string]bool{}
	last := map[string]attempt{}
	for _, a := range ex.results {
		if a.mode == modeParallel {
			last[a.id] = a
		}
	}
	for _, a := range ex.results {
		if a.mode != modeParallel || seen[a.id] {
			continue
		}
		seen[a.id] = true
		if l := last[a.id]; l.failed() && l.row.Status != "interrupted" {
			out = append(out, shard.TestID(a.id))
		}
	}
	return out
}

// passedAlone lists the tests among ids whose latest rerun passed.
func (ex *execution) passedAlone(ids []shard.TestID) []shard.TestID {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	last := map[string]attempt{}
	for _, a := range ex.results {
		if a.mode == modeRerun {
			last[a.id] = a
		}
	}
	var out []shard.TestID
	for _, id := range ids {
		if a, ok := last[string(id)]; ok && a.passed() {
			out = append(out, id)
		}
	}
	return out
}

package suite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/memo"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

var (
	errTimeout = errors.New("timed out")
	// errInfra ends a run's context when a service it needs dies: what is left to run proves nothing.
	errInfra = errors.New("infrastructure failure")
)

// execution is one run in progress.
type execution struct {
	r      *Runner
	j      *job
	suite  *Suite
	id     string
	ctx    context.Context // ends on cancel or timeout
	bg     context.Context // never ends: records written while stopping
	logDir string
	state  string // per-suite state directory

	hookToken string // what this run's shards send on every hook call

	dirs map[string]string // repo -> workspace
	shas map[string]string // repo -> the commit under test (guarded by mu)

	db  *dbSet
	iso *isoRun // isolation level state, nil for isolated
	wr  writeLearning

	mu       sync.Mutex
	attempts map[string]int // test id -> attempts so far in this run
	results  []attempt
	envOf    map[string]*shardEnv
	laneOf   map[string]int // test id -> the shard it first ran on
	layout   shard.Layout
	moved    int
	remote   map[string]bool // test id -> it runs on a worker now (guarded by mu)
	// localShards is how many of the layout's shards run here: the first ones, the rest go to workers.
	localShards int
	infra       string // why the stack or the tenant flow broke, "" while it did not (see setInfra)
	flaky       int    // tests that failed on the way and passed in the end
	tests       map[string]listed
	rec         *memo.Recorder // learns flow prefixes, nil when memo is off for the run
	plan        *memo.Planner  // hands tests to shards in prefix order, nil for the static layout

	mirrors     map[string]*exec.Mirror
	explainOnly bool
	selected    *selection // the listed and the run tests, once the run got that far

	badLines atomic.Int64 // report lines nobody could read

	// The suite's timeout runs only while the run holds capacity: time queued behind other runs is not the
	// suite's, or a busy machine would fail runs that never got to start a test.
	cancel  context.CancelCauseFunc
	tmu     sync.Mutex
	timer   *time.Timer
	left    time.Duration
	started time.Time
}

// resumeTimeout starts (or restarts) the clock for what is left of the suite's timeout.
func (ex *execution) resumeTimeout() {
	d := ex.suite.Timeout
	if d <= 0 {
		return
	}
	ex.tmu.Lock()
	defer ex.tmu.Unlock()
	if ex.timer == nil {
		ex.left = d
		ex.timer = time.AfterFunc(d, func() { ex.cancel(errTimeout) })
	} else {
		ex.timer.Reset(max(ex.left, time.Millisecond))
	}
	ex.started = time.Now()
}

// pauseTimeout stops the clock while the run waits for capacity.
func (ex *execution) pauseTimeout() {
	ex.tmu.Lock()
	defer ex.tmu.Unlock()
	if ex.timer != nil && ex.timer.Stop() {
		ex.left -= time.Since(ex.started)
	}
}

func (ex *execution) stopTimeout() {
	ex.tmu.Lock()
	defer ex.tmu.Unlock()
	if ex.timer != nil {
		ex.timer.Stop()
	}
}

type listed struct {
	File, Title string
	Line        int
}

func (r *Runner) execute(j *job) {
	ex := &execution{r: r, j: j, suite: j.suite, id: j.id, bg: context.Background(),
		attempts: map[string]int{}, envOf: map[string]*shardEnv{}, laneOf: map[string]int{}, dirs: map[string]string{},
		mirrors: map[string]*exec.Mirror{}, shas: map[string]string{}}
	ex.logDir = filepath.Join(r.d.DataDir, "logs", j.id)
	ex.state = filepath.Join(r.d.DataDir, "state", slug(j.suite.Name))
	ex.hookToken = newHookToken()

	r.mu.Lock()
	r.live[j.id] = ex
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, j.id)
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithCancelCause(j.ctx)
	defer cancel(nil)
	ex.ctx, ex.cancel = ctx, cancel
	defer ex.stopTimeout()

	if j.req.Explore != nil {
		r.executeExplore(ex)
		return
	}
	err := ex.run()
	ex.triage() // it classifies the interrupted tests as infrastructure, with the failure as evidence
	state, reason := ex.outcome(err)
	if err == nil && ctx.Err() == nil && ex.selected != nil {
		ex.affectedDone(ex.selected.all, ex.selected.ran)
	}
	r.finishRun(j.id, state, reason)
}

// outcome turns how run ended into the run's final state.
func (ex *execution) outcome(err error) (string, string) {
	if c := context.Cause(ex.j.ctx); c != nil && ex.j.ctx.Err() != nil {
		reason := "cancelled"
		var sc stopCause
		if errors.As(c, &sc) {
			reason = string(sc)
		}
		return store.StateCancelled, reason
	}
	if ex.ctx.Err() != nil && errors.Is(context.Cause(ex.ctx), errTimeout) {
		return store.StateFailed, fmt.Sprintf("timed out after %s", ex.suite.Timeout)
	}
	if infra := ex.infraReason(); infra != "" {
		return store.StateFailed, "infrastructure: " + infra
	}
	if err != nil {
		return store.StateFailed, trim(err.Error(), 600)
	}
	failures, _ := ex.r.d.Store.Failures(ex.bg, ex.id)
	moved := ex.movedCount()
	note := ""
	if moved > 0 {
		note = fmt.Sprintf("%d moved to serial group", moved)
	}
	if n := ex.flakyNote(); n != "" {
		if note != "" {
			note += "; "
		}
		note += n
	}
	if bad := ex.badLines.Load(); bad > 0 {
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("%d report lines could not be read", bad)
	}
	ex.mu.Lock()
	total := len(ex.tests)
	ex.mu.Unlock()
	switch {
	case len(failures) > 0:
		reason := fmt.Sprintf("%d of %d tests failed", len(failures), total)
		if note != "" {
			reason += "; " + note
		}
		return store.StateFailed, reason
	case ex.explainOnly:
		return store.StatePassed, "explain only: no test ran"
	case total == 0 && ex.j.req.Affected != nil:
		return store.StatePassed, "no tests affected"
	case total == 0:
		return store.StatePassed, "no tests matched"
	}
	return store.StatePassed, note
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (ex *execution) emit(shardN int, kind, test, step string, data any) {
	var raw json.RawMessage
	switch v := data.(type) {
	case nil:
	case json.RawMessage:
		raw = v
	default:
		raw, _ = json.Marshal(v)
	}
	ex.r.d.Log.Append(ex.bg, store.Event{Run: ex.id, Shard: shardN, Kind: kind, Test: test, Step: step, Data: raw})
}

func (ex *execution) setState(state string) {
	ex.r.d.Store.SetRunState(ex.bg, ex.id, state, "")
	ex.emit(0, "run.state", "", "", map[string]string{"state": state})
}

// run is the whole flow: admission, checkout, prepare, template, list, plan, shards, serial group, reruns.
func (ex *execution) run() error {
	s, d := ex.suite, ex.r.d
	if err := os.MkdirAll(ex.logDir, 0o755); err != nil {
		return err
	}

	// One slot while the repos are prepared; an exclusive suite holds the whole machine for the whole run.
	var release func()
	var err error
	if s.Exclusive {
		release, err = d.Sched.AdmitExclusive(ex.ctx)
	} else {
		release, err = d.Sched.Admit(ex.ctx, sched.Cost{CPUs: 1, Memory: shard.DefaultCost.Memory})
	}
	if err != nil {
		return err
	}
	defer func() { release() }()
	ex.resumeTimeout()
	ex.setState(store.StatePreparing)

	if err := ex.checkout(); err != nil {
		return fmt.Errorf("checkout: %w", err)
	}
	if err := ex.prepare(); err != nil {
		return err
	}
	if err := ex.setupDB(); err != nil {
		return err
	}
	all, err := ex.listTests()
	if err != nil {
		return err
	}
	if len(all) == 0 && ex.j.req.Tests.empty() {
		return errors.New("the list command listed no tests (is the framework package installed and the test directory right?)")
	}
	tests, err := ex.applyFilter(all)
	if err != nil {
		return err
	}
	if ex.j.req.Affected != nil {
		if tests, err = ex.selectAffected(all, tests); err != nil {
			return err
		}
	}
	ex.selected = &selection{all: all, ran: tests}
	ex.mu.Lock()
	ex.tests = map[string]listed{}
	for _, t := range tests {
		ex.tests[t.id] = listed{t.file, t.title, t.line}
	}
	ex.mu.Unlock()

	// Plan.
	ids := make([]shard.TestID, len(tests))
	for i, t := range tests {
		ids[i] = shard.TestID(t.id)
	}
	medians, serial, err := ex.history(ids)
	if err != nil {
		return err
	}
	per := ex.shardCost()
	n := s.Shards
	// The shards past the first localN go to workers: every slot they offer when the count is automatic, and with
	// a fixed count all but at least one, which stays here.
	remoteN := ex.remoteSlots()
	localN := n
	if n == 0 {
		parallel := 0
		for _, id := range ids {
			if !serial[id] {
				parallel++
			}
		}
		c := d.Sched.Capacity()
		localN = shard.Count(capacity.Machine{CPUs: c.CPUs, FreeMemory: c.Memory}, shard.Cost{CPUs: per.CPUs, Memory: per.Memory}, parallel)
		n = localN + remoteN
	} else {
		remoteN = min(remoteN, n-1)
		localN = n - remoteN
	}
	ex.layout = shard.Plan(ids, medians, n, serial)
	ex.localShards = min(localN, len(ex.layout.Shards))
	if err := ex.applyLevel(ids, medians, serial, n); err != nil {
		return err
	}
	ex.emit(0, "run.planned", "", "", map[string]any{
		"tests": len(ids), "shards": len(ex.layout.Shards), "serial": len(ex.layout.Serial),
		"makespan_ms": ex.layout.Makespan.Milliseconds(), "alone_ms": ex.layout.Alone.Milliseconds(),
	})
	if len(ids) == 0 {
		return nil
	}

	// The shards need the machine: give back the preparation slot first so waiting never holds capacity.
	if !s.Exclusive {
		release()
		count := max(ex.localShards, 1)
		ex.pauseTimeout()
		release, err = d.Sched.Admit(ex.ctx, sched.Cost{CPUs: count * per.CPUs, Memory: uint64(count) * per.Memory})
		ex.resumeTimeout()
		if err != nil {
			release = func() {}
			return err
		}
	}
	ex.setState(store.StateRunning)

	// Parallel part.
	if err := ex.startShared(ex.ctx); err != nil {
		return err
	}
	defer ex.dropShared()
	ex.memoStart(ids)
	_, plan := ex.memoState()
	errs := make([]error, len(ex.layout.Shards))
	var wg sync.WaitGroup
	for i, tids := range ex.layout.Shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i >= ex.localShards {
				errs[i] = ex.runRemoteShard(i+1, tids)
				return
			}
			if plan != nil {
				errs[i] = ex.runMemoShard(i+1, plan)
				return
			}
			errs[i] = ex.runShard(i+1, tids)
		}()
	}
	wg.Wait()
	ex.closeShared()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if ex.ctx.Err() != nil {
		return ex.ctx.Err()
	}

	// Serial group and reruns, alone on shard 0.
	if err := ex.runAlone(); err != nil {
		return err
	}
	if ex.ctx.Err() != nil {
		return ex.ctx.Err()
	}
	return ex.finalize()
}

// selection is what the run listed and what it runs.
type selection struct{ all, ran []listedTest }

type listedTest struct {
	id, file, title string
	line            int
}

// listTests runs the suite's list command in the test repo.
func (ex *execution) listTests() ([]listedTest, error) {
	s := ex.suite
	log := filepath.Join(ex.logDir, "list.log")
	res, err := exec.Run(ex.ctx, exec.Spec{Cmd: s.Test.List, Dir: ex.dirs[s.Test.Repo], LogPath: log, KillGrace: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		if ex.ctx.Err() != nil {
			return nil, ex.ctx.Err()
		}
		return nil, fmt.Errorf("listing tests failed (exit %d): %s", res.ExitCode, strings.Join(tail(log, 8), "\n"))
	}
	var out []listedTest
	seen := map[string]bool{}
	for _, line := range readLines(log) {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var t struct {
			Test  string `json:"test"`
			File  string `json:"file"`
			Title string `json:"title"`
			Line  int    `json:"line"`
		}
		if json.Unmarshal([]byte(line), &t) != nil || t.Test == "" || seen[t.Test] {
			continue
		}
		seen[t.Test] = true
		out = append(out, listedTest{t.Test, t.File, t.Title, t.Line})
	}
	return out, nil
}

func compileGrep(expr string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("grep: %w", err)
	}
	return re, nil
}

// applyFilter keeps the listed tests the request asks for.
func (ex *execution) applyFilter(all []listedTest) ([]listedTest, error) {
	f := ex.j.req.Tests
	if f.empty() {
		return all, nil
	}
	var re *regexp.Regexp
	if f.Grep != "" {
		var err error
		if re, err = compileGrep(f.Grep); err != nil {
			return nil, err
		}
	}
	var failed map[string]bool
	if f.FailedOf != "" {
		fails, err := ex.r.d.Store.Failures(ex.bg, f.FailedOf)
		if err != nil {
			return nil, err
		}
		failed = map[string]bool{}
		for _, t := range fails {
			failed[t.ID] = true
		}
	}
	var out []listedTest
	for _, t := range all {
		switch {
		case len(f.IDs) > 0 && !slices.Contains(f.IDs, t.id):
		case re != nil && !re.MatchString(t.file+" "+t.title):
		case len(f.Files) > 0 && !matchesFile(f.Files, t.file):
		case failed != nil && !failed[t.id]:
		default:
			out = append(out, t)
		}
	}
	return out, nil
}

func matchesFile(files []string, file string) bool {
	for _, f := range files {
		f = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(f)), "/")
		if file == f || strings.HasSuffix(file, "/"+f) || strings.HasPrefix(file, f+"/") {
			return true
		}
	}
	return false
}

// history loads what earlier runs taught: durations and the serial group.
func (ex *execution) history(ids []shard.TestID) (map[shard.TestID]time.Duration, map[shard.TestID]bool, error) {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = string(id)
	}
	ms, err := ex.r.d.Store.MedianDurations(ex.bg, strs)
	if err != nil {
		return nil, nil, err
	}
	medians := map[shard.TestID]time.Duration{}
	for id, v := range ms {
		medians[shard.TestID(id)] = time.Duration(v) * time.Millisecond
	}
	entries, err := ex.r.d.Store.Serial(ex.bg, ex.suite.Name)
	if err != nil {
		return nil, nil, err
	}
	serial := map[shard.TestID]bool{}
	for _, e := range entries {
		serial[shard.TestID(e.Test)] = true
	}
	return medians, serial, nil
}

// shardCost is what one shard of this suite needed last time, else the default.
func (ex *execution) shardCost() sched.Cost {
	runs, err := ex.r.d.Store.Runs(ex.bg, 0)
	if err == nil {
		seen := 0
		for _, run := range runs {
			if run.Suite != ex.suite.Name || run.Kind != store.KindSuite || run.ID == ex.id {
				continue
			}
			if seen++; seen > 20 {
				break
			}
			shards, err := ex.r.d.Store.Shards(ex.bg, run.ID)
			if err != nil {
				continue
			}
			var cpu float64
			var mem int64
			for _, sh := range shards {
				cpu, mem = max(cpu, sh.CostCPU), max(mem, sh.CostMem)
			}
			if mem > 0 {
				return sched.Cost{CPUs: max(int(cpu+0.999), 1), Memory: uint64(mem)}
			}
		}
	}
	return sched.Cost{CPUs: shard.DefaultCost.CPUs, Memory: shard.DefaultCost.Memory}
}

func (ex *execution) putShard(n int, state string, log string, cost sched.Cost, cpu float64) {
	sh := store.Shard{Run: ex.id, N: n, State: state, LogPath: log, CostCPU: cpu, CostMem: int64(cost.Memory)}
	ex.r.d.Store.PutShard(ex.bg, sh)
	ex.emit(n, "shard.state", "", "", map[string]string{"state": state})
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteString("__")
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

func tail(path string, n int) []string {
	l := readLines(path)
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return l
}

package suite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
)

// Head is the commit of one repo a run tests. Dir, when set, is a checkout whose working tree (uncommitted tracked
// changes included) is tested instead of a published commit.
type Head struct {
	forge.Head
	Dir string `json:"dir,omitempty"`
	// Dirty says Dir held uncommitted tracked changes when the run was asked for; SHA is its HEAD then.
	Dirty bool `json:"dirty,omitempty"`
}

// Filter narrows a run to some tests; every field that is set must match.
type Filter struct {
	IDs      []string // exact test ids
	Grep     string   // regular expression matched against "file title"
	Files    []string // test files or directories, relative to the test repo
	FailedOf string   // a run id: the tests that failed in it
}

func (f Filter) empty() bool {
	return len(f.IDs) == 0 && f.Grep == "" && len(f.Files) == 0 && f.FailedOf == ""
}

// Request asks for a run of a suite.
type Request struct {
	Suite       string
	Heads       []Head // missing repos use their default branch
	Tests       Filter
	Trigger     string
	RequestedBy string
	// Fork says the heads include a pull request head from another repository.
	Fork bool
	// Affected, when set, runs only the tests the diff can affect (see Affector).
	Affected *AffectedRequest
	// Explore, when set, makes this an exploratory run (see ExploreRequest): default branches only, never a pull
	// request, no tests.
	Explore *ExploreRequest
}

// Deps is everything a Runner needs from the outside.
type Deps struct {
	Store   *store.Store
	Log     *event.Log
	Sched   *sched.Scheduler
	DataDir string // mirrors, workspaces, logs and per-suite state live below it
	Suites  []*Suite
	// RepoURL says where a repo is cloned from; the default is its GitHub address.
	RepoURL func(repo string) string
	// API is the base URL Handler is served at; shards get it as $LOCALCI_API (with the run appended). Empty
	// means no hooks.
	API string
	// NewID makes run ids; the default counts up from the highest numeric id in the store. A process that also
	// creates runs elsewhere (checks) passes one shared source.
	NewID func() string
	// Remote hands shards to workers on other machines; nil keeps every shard here. RemoteForks lets runs of pull
	// requests from forks use them (off: a fork's code would run on another machine).
	Remote      Remote
	RemoteForks bool
}

// Runner runs suites. Create one per process.
type Runner struct {
	d      Deps
	suites map[string]*Suite
	base   context.Context
	stop   context.CancelFunc
	wg     sync.WaitGroup
	nextID atomic.Int64

	mu     sync.Mutex
	queues map[string]*suiteQueue
	jobs   map[string]*job // runs that have not finished
	live   map[string]*execution
	hooks  []hook
	areas  map[string]HookFunc

	affector Affector
}

type suiteQueue struct {
	waiting []*job
	running *job
	working bool
}

type job struct {
	id     string
	suite  *Suite
	req    Request
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// stopCause is why a run was told to stop; it becomes the run's reason.
type stopCause string

func (c stopCause) Error() string { return string(c) }

const (
	causeCancelled  = stopCause("cancelled")
	causeSuperseded = stopCause("superseded")
	causeShutdown   = stopCause("shutdown")
)

// New returns a runner for the suites in d.
func New(d Deps) (*Runner, error) {
	switch {
	case d.Store == nil || d.Log == nil || d.Sched == nil:
		return nil, errors.New("suite: Store, Log and Sched are required")
	case d.DataDir == "":
		return nil, errors.New("suite: DataDir is required")
	}
	if d.RepoURL == nil {
		d.RepoURL = func(repo string) string { return "https://github.com/" + repo + ".git" }
	}
	base, stop := context.WithCancel(context.Background())
	r := &Runner{d: d, suites: map[string]*Suite{}, base: base, stop: stop,
		queues: map[string]*suiteQueue{}, jobs: map[string]*job{}, live: map[string]*execution{}}
	for _, s := range d.Suites {
		if r.suites[s.Name] != nil {
			stop()
			return nil, fmt.Errorf("suite: %q is defined twice", s.Name)
		}
		r.suites[s.Name] = s
	}
	r.OnTest(r.isoBegin, r.isoEnd)
	r.HandleHook("memo", r.memoNext)
	runs, err := d.Store.Runs(base, 0)
	if err != nil {
		stop()
		return nil, err
	}
	for _, run := range runs {
		if n, err := strconv.ParseInt(run.ID, 10, 64); err == nil && n > r.nextID.Load() {
			r.nextID.Store(n)
		}
	}
	return r, nil
}

func (r *Runner) newID() string {
	if r.d.NewID != nil {
		return r.d.NewID()
	}
	return strconv.FormatInt(r.nextID.Add(1), 10)
}

// Suite returns a configured suite.
func (r *Runner) Suite(name string) (*Suite, bool) { s, ok := r.suites[name]; return s, ok }

// Submit queues a run of req.Suite and returns its id at once. A suite runs one run at a time (its workspaces
// are warm and shared); a newer request for the same pull requests cancels the older run, queued or running,
// with the reason "superseded".
func (r *Runner) Submit(req Request) (string, error) {
	s := r.suites[req.Suite]
	if s == nil {
		return "", fmt.Errorf("unknown suite %q", req.Suite)
	}
	for _, h := range req.Heads {
		if !s.hasRepo(h.Repo) {
			return "", fmt.Errorf("suite %s has no repository %q", s.Name, h.Repo)
		}
	}
	if req.Tests.Grep != "" {
		if _, err := compileGrep(req.Tests.Grep); err != nil {
			return "", err
		}
	}
	if req.Explore != nil && (len(req.Heads) > 0 || req.Fork || req.Affected != nil || !req.Tests.empty()) {
		return "", errors.New("an exploratory run takes the default branches only: no heads, filter or affected")
	}
	if err := r.checkAffected(req); err != nil {
		return "", err
	}
	if err := r.base.Err(); err != nil {
		return "", errors.New("suite: the runner is shut down")
	}

	id := r.newID()
	heads, _ := json.Marshal(req.Heads)
	ctx := context.Background()
	if err := r.d.Store.PutRun(ctx, store.Run{ID: id, Suite: s.Name, Kind: store.KindSuite, Heads: heads,
		Trigger: req.Trigger, RequestedBy: req.RequestedBy, State: store.StateQueued, Fork: req.Fork,
		Affected: req.Affected != nil}); err != nil {
		return "", err
	}
	jctx, cancel := context.WithCancelCause(r.base)
	j := &job{id: id, suite: s, req: req, ctx: jctx, cancel: cancel, done: make(chan struct{})}
	r.appendEvent(store.Event{Run: id, Kind: "run.state", Data: stateData(store.StateQueued, "")})

	r.mu.Lock()
	q := r.queues[s.Name]
	if q == nil {
		q = &suiteQueue{}
		r.queues[s.Name] = q
	}
	var running *job    // superseded while running: it finishes itself
	var dequeued []*job // superseded before starting: off the queue here, finished below
	if key := supersedeKey(req); key != "" {
		if q.running != nil && supersedeKey(q.running.req) == key {
			running = q.running
		}
		kept := q.waiting[:0]
		for _, w := range q.waiting {
			if supersedeKey(w.req) == key {
				dequeued = append(dequeued, w)
			} else {
				kept = append(kept, w)
			}
		}
		q.waiting = kept
	}
	q.waiting = append(q.waiting, j)
	r.jobs[id] = j
	start := !q.working
	q.working = true
	r.mu.Unlock()

	if running != nil {
		r.stopJob(running, causeSuperseded)
	}
	for _, old := range dequeued {
		r.endQueued(old, causeSuperseded)
	}
	if start {
		r.wg.Add(1)
		go r.work(s.Name)
	}
	return id, nil
}

// Cancel stops a queued or running run. It reports whether the run was still active.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j == nil {
		return false
	}
	r.stopJob(j, causeCancelled)
	return true
}

// stopJob cancels a job. One that has not started is finished here; a running one finishes itself, after its
// teardown.
func (r *Runner) stopJob(j *job, cause stopCause) {
	r.mu.Lock()
	q := r.queues[j.suite.Name]
	idx := slices.Index(q.waiting, j)
	if idx >= 0 {
		q.waiting = slices.Delete(q.waiting, idx, idx+1)
	}
	r.mu.Unlock()
	if idx >= 0 {
		r.endQueued(j, cause)
		return
	}
	j.cancel(cause)
}

// endQueued finishes a job that was taken off its queue before it started. It stays known until its state is
// stored, so a Wait in between does not read it as still queued.
func (r *Runner) endQueued(j *job, cause stopCause) {
	j.cancel(cause)
	r.finishRun(j.id, store.StateCancelled, string(cause), "")
	r.mu.Lock()
	delete(r.jobs, j.id)
	r.mu.Unlock()
	close(j.done)
}

// work runs a suite's queue, one run at a time.
func (r *Runner) work(suite string) {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		q := r.queues[suite]
		if len(q.waiting) == 0 {
			q.working, q.running = false, nil
			r.mu.Unlock()
			return
		}
		j := q.waiting[0]
		q.waiting = q.waiting[1:]
		q.running = j
		r.mu.Unlock()

		r.execute(j)

		r.mu.Lock()
		delete(r.jobs, j.id)
		r.mu.Unlock()
		close(j.done)
	}
}

// Wait returns the run once it is finished.
func (r *Runner) Wait(ctx context.Context, id string) (store.Run, error) {
	r.mu.Lock()
	j := r.jobs[id]
	r.mu.Unlock()
	if j != nil {
		select {
		case <-j.done:
		case <-ctx.Done():
			return store.Run{}, ctx.Err()
		}
	}
	return r.d.Store.Run(ctx, id)
}

// Close cancels every run and waits until they have torn down.
func (r *Runner) Close() {
	r.mu.Lock()
	var all []*job
	for _, j := range r.jobs {
		all = append(all, j)
	}
	r.mu.Unlock()
	for _, j := range all {
		r.stopJob(j, causeShutdown)
	}
	r.stop()
	r.wg.Wait()
}

// finishRun records a terminal state and tells the log. outcome is the run's outcome code, usually "".
func (r *Runner) finishRun(id, state, reason, outcome string) {
	ctx := context.Background()
	if err := r.d.Store.SetRunOutcome(ctx, id, state, reason, outcome); err != nil {
		return
	}
	r.appendEvent(store.Event{Run: id, Kind: "run.state", Data: stateData(state, reason)})
}

func (r *Runner) appendEvent(e store.Event) {
	r.d.Log.Append(context.Background(), e)
}

func stateData(state, reason string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"state": state, "reason": reason})
	return b
}

// prKey identifies the pull requests a run is about; "" when it is about none.
func prKey(heads []Head) string {
	var parts []string
	for _, h := range heads {
		if h.PR > 0 {
			parts = append(parts, h.Repo+"#"+strconv.Itoa(h.PR))
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// supersedeKey is what a newer run replaces: the same pull requests in the same mode. A full run does not
// replace an affected one (their statuses differ, the affected one would be left pending).
func supersedeKey(req Request) string {
	key := prKey(req.Heads)
	if key != "" && req.Affected != nil {
		key += " (affected)"
	}
	return key
}

// Package checks runs the pull request checks of a repo's `.github/workflows`
// on this machine: each planned job becomes a run of kind `check`, its steps
// run in order in a workspace at the pull request's merge result, and each
// check reports its own commit status, and `localci/checks` sums them up. A tree
// already proven green by the same check is not run again.
package checks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/workflow"
)

// Cost is what a check asks the scheduler for before it starts.
type Cost struct {
	CPU float64 // cores
	Mem int64   // bytes
}

// Admitter lets a check start once the machine has room for it. Admit blocks
// until then; release gives the room back.
type Admitter interface {
	Admit(ctx context.Context, c Cost) (release func(), err error)
}

// Options are the settings checks read from the configuration.
type Options struct {
	// DataDir holds mirrors, workspaces and logs.
	DataDir string
	// MergeRef tests the pull request's merge result instead of its head
	// (`fidelity.merge_ref`); PathFilters honours `paths` (`fidelity.path_filters`).
	MergeRef, PathFilters bool
	// Toolchains is `fidelity.toolchains`: "mise", "containers" or "" for auto.
	Toolchains string
	// DockerCache gives `docker run` lines of a step a cache volume
	// (`fidelity.docker_cache`).
	DockerCache bool
	// DefaultImage runs jobs that need a container (they have services) but
	// name no image and need no tools. Default ubuntu:24.04.
	DefaultImage string
	// Cost is what one check asks for. Default one core and 1 GiB.
	Cost Cost
	// Keep lists workspace paths that survive from one run to the next.
	Keep []string
	// SkipWorkflows maps a repo to the workflow files it never runs
	// (`repos[].workflows.skip`), by file name ("release.yml") or repo path.
	SkipWorkflows map[string][]string
	// RemoteURL is where a repo's mirror is cloned from. Default GitHub over https.
	RemoteURL func(repo string) string
	// URL is the dashboard page of a run, linked from its statuses.
	URL func(runID string) string
}

// ActJob is a check that needs third-party actions, handed to Runner.Act.
type ActJob struct {
	Check   workflow.Check
	Dir     string // the workspace, at the tree under test
	LogPath string
	Request Request
	// Merge is the commit under test; Request.Head is the one statuses go to.
	Merge string
	// Run is the id of the check's run, the label its containers carry.
	Run string
}

// Runner plans and runs checks. The zero value is not usable: set Store,
// Admit and Options.DataDir; Forge, Log, Act and Mirror are optional.
type Runner struct {
	Options Options
	Store   *store.Store
	Admit   Admitter
	// Forge receives the commit statuses; nil posts none.
	Forge forge.Forge
	// Log receives the run and step events; nil records none.
	Log *event.Log
	// Act runs checks that need third-party actions (NeedsActions). Without
	// it they are left out of Submit's runs, with the reason.
	Act func(ctx context.Context, j ActJob) (exec.Result, error)
	// Mirror returns the mirror of a repo. Default: clone from Options.RemoteURL.
	Mirror func(ctx context.Context, repo string) (*exec.Mirror, error)
	// Context bounds every run Submit starts. Default context.Background().
	Context context.Context

	tags    tagLocks
	wsLocks sync.Map // workspace dir -> *sync.Mutex
	waiting atomic.Int64
	wg      sync.WaitGroup
	mu      sync.Mutex
	active  map[string]*job // by run id, and by repo, PR and check
	batch   map[*job]string // the Submit that created each active job
	// summaries has a *headSummary per repo and head commit; newest maps a pull
	// request to the last head Submit planned for it.
	summaries, newest sync.Map
	ctxOnce           sync.Once
	baseCtx           context.Context
}

// Request asks for the checks of one tree.
type Request struct {
	Repo string // owner/name
	// PR, Base and Head describe a pull request: its number, the branch it
	// targets and its head commit, the one statuses attach to.
	PR   int
	Base string
	Head string
	// Snapshot is the commit of a working tree that Runner.Snapshot recorded;
	// set it instead of PR and Head to check uncommitted work. No statuses.
	Snapshot    string
	Trigger     string // push | label | manual
	RequestedBy string
	// Force runs the checks even when their tree already passed.
	Force bool
	// LastAttempt is set on the watcher's last try for this head: if GitHub's
	// merge ref is still not ready, Submit posts an error status on the head
	// instead of leaving the pull request without checks in silence.
	LastAttempt bool
	// Only, when set, starts just the check with this status context
	// (`localci/<workflow>/<name>`). Every check is still planned and named
	// first, so a suffixed name is found. A request for a context no check
	// has starts nothing.
	Only string
}

// Started is one check Submit began.
type Started struct {
	RunID    string
	Workflow string
	Name     string
	// Context is the commit status context, `localci/<workflow>/<job>`.
	Context string
	// Done is closed when the run reached a terminal state.
	Done <-chan struct{}
}

// Submission is what Submit started and what it left out.
type Submission struct {
	Runs    []Started
	Skipped []workflow.Skip
}

var errSuperseded = errors.New("superseded")

// Submit plans the checks of the tree in req and starts them in the
// background; it returns once they are queued. For a pull request it also posts
// `localci/checks`: pending when checks start, success when none applies. A pull
// request that does not merge into its base has no merge result to test: that is
// a *exec.MergeConflictError and nothing starts.
func (r *Runner) Submit(ctx context.Context, req Request) (Submission, error) {
	if req.Repo == "" || (req.Head == "" && req.Snapshot == "") {
		return Submission{}, errors.New("checks: a request needs a repo and a head or a snapshot")
	}
	m, err := r.mirror(ctx, req.Repo)
	if err != nil {
		return Submission{}, err
	}
	sha, err := r.resolve(ctx, m, req)
	if err != nil {
		var pe *PullRefError
		if req.LastAttempt && r.Forge != nil && errors.As(err, &pe) && pe.MergeRef() {
			// Nothing was planned, so there is no check context to report on:
			// one status on the head says why its checks did not run.
			st := forge.Status{Suite: summarySuite, State: forge.StateError, Description: "GitHub merge ref not ready"}
			if perr := r.Forge.SetStatus(ctx, req.Repo, req.Head, st); perr != nil {
				err = errors.Join(err, perr)
			}
		}
		return Submission{}, err
	}
	ev := workflow.Event{Name: "pull_request", Repo: req.Repo, Branch: req.Base}
	if r.Options.PathFilters && req.Base != "" {
		if _, err := m.FetchBranch(ctx, req.Base); err != nil {
			return Submission{}, err
		}
		ev.BaseRef = "refs/heads/" + req.Base
	}
	plan, err := r.plan(ctx, m.Dir, sha, ev)
	if err != nil {
		return Submission{}, err
	}
	tree, err := gitOut(ctx, m.Dir, "rev-parse", "--verify", sha+"^{tree}")
	if err != nil {
		return Submission{}, err
	}

	sub := Submission{Skipped: plan.Skipped}
	var runnable []workflow.Check
	for _, c := range plan.Checks {
		if c.NeedsActions && r.Act == nil {
			sub.Skipped = append(sub.Skipped, workflow.Skip{Workflow: c.Workflow, Job: c.Name,
				Reason: "needs third-party actions and `act` is not set up"})
			continue
		}
		runnable = append(runnable, c)
	}
	// Every name is final before the first status is created: two checks that
	// shared a context would overwrite each other's status on the commit.
	runnable = uniqueNames(runnable)
	var planned, starting []string
	var selected []workflow.Check
	for _, c := range runnable {
		planned = append(planned, checkName(c))
		if req.Only == "" || forge.Context(checkName(c)) == req.Only {
			starting = append(starting, checkName(c))
			selected = append(selected, c)
		}
	}
	var summary *headSummary
	if req.PR > 0 && r.Forge != nil {
		summary = r.headSummary(req.Repo, req.Head)
		summary.mu.Lock()
		defer summary.mu.Unlock()
		summary.planned = planned
		// Set before a check of this head supersedes one of an older head: from
		// here on that one gets no summary.
		r.newest.Store(pullKey(req), req.Head)
		// Posted before any check starts, so a result can only come after it.
		// With nothing planned this is the only one, and it must be posted: a
		// required status that never arrives blocks the pull request for ever.
		if len(starting) > 0 || len(planned) == 0 {
			r.postSummary(req, planned, starting)
		}
	}
	batch := newID()
	for _, c := range selected {
		j := &job{r: r, req: req, check: c, mirror: m, sha: sha, tree: tree, id: newID(), done: make(chan struct{})}
		j.name = checkName(c)
		if err := j.create(ctx); err != nil {
			if summary != nil {
				// The rest will not start: the pending one above must not wait for them.
				r.postSummary(req, planned, nil)
			}
			return sub, err
		}
		sub.Runs = append(sub.Runs, Started{RunID: j.id, Workflow: c.Workflow, Name: c.Name, Context: forge.Context(j.name), Done: j.done})
		r.start(j, batch)
	}
	return sub, nil
}

// checkName is `<workflow>/<job>`, the check's status context without its
// prefix.
func checkName(c workflow.Check) string { return workflowLabel(c.Workflow) + "/" + c.Name }

// uniqueNames gives every check of cs its own status context
// (`localci/<workflow>/<name>`, across workflows). A check whose context is
// shared with another gets GitHub's cell suffix (`test linux (linux, 18)`);
// names that are unique keep their name, so contexts of existing setups do not
// change. Cells that still collide are numbered: `(linux, 18 #2)`.
func uniqueNames(cs []workflow.Check) []workflow.Check {
	count := map[string]int{}
	for _, c := range cs {
		count[checkName(c)]++
	}
	out := slices.Clone(cs)
	for i, c := range out {
		if count[checkName(c)] > 1 {
			out[i].Name += c.Cell
		}
	}
	seen := map[string]int{}
	for i, c := range out {
		k := checkName(c)
		seen[k]++
		if n := seen[k]; n > 1 {
			if base, ok := strings.CutSuffix(c.Name, ")"); ok {
				out[i].Name = fmt.Sprintf("%s #%d)", base, n)
			} else {
				out[i].Name = fmt.Sprintf("%s (#%d)", c.Name, n)
			}
		}
	}
	return out
}

// plan is workflow.PlanDetailed without the workflow files the repo skips
// (Options.SkipWorkflows): they are not even read, and show as skipped.
func (r *Runner) plan(ctx context.Context, dir, sha string, ev workflow.Event) (workflow.Result, error) {
	skip := r.Options.SkipWorkflows[ev.Repo]
	if len(skip) == 0 {
		return workflow.PlanDetailed(dir, sha, ev)
	}
	if ev.Files == nil && ev.BaseRef != "" {
		files, err := workflow.ChangedFiles(dir, ev.BaseRef, sha)
		if err != nil {
			return workflow.Result{}, err
		}
		ev.Files = files
	}
	list, err := gitOut(ctx, dir, "ls-tree", "-r", "--name-only", sha, "--", ".github/workflows")
	if err != nil {
		return workflow.Result{}, fmt.Errorf("listing workflows at %s: %w", sha, err)
	}
	files := map[string][]byte{}
	var skipped []workflow.Skip
	for _, p := range strings.Split(list, "\n") {
		if ext := path.Ext(p); path.Dir(p) != ".github/workflows" || (ext != ".yml" && ext != ".yaml") {
			continue
		}
		if slices.ContainsFunc(skip, func(s string) bool { return s == p || s == path.Base(p) }) {
			skipped = append(skipped, workflow.Skip{Workflow: p, Reason: "listed in repos[].workflows.skip"})
			continue
		}
		b, err := gitBytes(ctx, dir, "show", sha+":"+p)
		if err != nil {
			return workflow.Result{}, fmt.Errorf("reading %s at %s: %w", p, sha, err)
		}
		files[p] = b
	}
	res := workflow.PlanWorkflows(sha, ev, files)
	res.Skipped = append(skipped, res.Skipped...)
	return res, nil
}

// Snapshot records the tracked state of the checkout in dir, uncommitted
// changes included, in the repo's mirror, for Request.Snapshot. The mirror of
// a repo with none yet is cloned from dir itself.
func (r *Runner) Snapshot(ctx context.Context, repo, dir string) (exec.Snapshot, error) {
	m, err := r.mirrorFrom(ctx, repo, dir)
	if err != nil {
		return exec.Snapshot{}, err
	}
	return m.Snapshot(ctx, dir)
}

// Wait blocks until every run Submit started is done and its statuses, the
// summary included, are posted.
func (r *Runner) Wait() { r.wg.Wait() }

// Cancel stops the running or queued run id.
func (r *Runner) Cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j := r.active[id]; j != nil {
		j.cancel(context.Canceled)
	}
}

func (r *Runner) mirror(ctx context.Context, repo string) (*exec.Mirror, error) {
	return r.mirrorFrom(ctx, repo, "")
}

func (r *Runner) mirrorFrom(ctx context.Context, repo, local string) (*exec.Mirror, error) {
	if r.Mirror != nil {
		return r.Mirror(ctx, repo)
	}
	url := "https://github.com/" + repo + ".git"
	if r.Options.RemoteURL != nil {
		url = r.Options.RemoteURL(repo)
	}
	if local != "" {
		url = local
	}
	return exec.EnsureMirror(ctx, r.Options.DataDir, repo, url)
}

// resolve fetches what the request names and returns the commit to test.
func (r *Runner) resolve(ctx context.Context, m *exec.Mirror, req Request) (string, error) {
	switch {
	case req.Snapshot != "":
		return req.Snapshot, nil
	case req.PR > 0:
		// The pull request's refs only tell what GitHub has now: test them only
		// when they are at the requested head (statuses go to req.Head). When
		// they are behind, the caller tries again later (the watcher's next
		// poll); nothing waits here.
		return r.fetchPull(ctx, m, req)
	default:
		if err := m.Fetch(ctx, req.Head); err != nil {
			return "", err
		}
		return gitOut(ctx, m.Dir, "rev-parse", "--verify", req.Head+"^{commit}")
	}
}

// fetchPull fetches pull request req.PR and returns the commit to test for
// req.Head: its merge commit (Options.MergeRef) or the head itself. Refs that
// are not at req.Head are a *PullRefError.
func (r *Runner) fetchPull(ctx context.Context, m *exec.Mirror, req Request) (string, error) {
	headRef := fmt.Sprintf("refs/pull/%d/head", req.PR)
	if r.Options.MergeRef {
		pr, err := m.FetchPull(ctx, req.PR, req.Base)
		if err != nil {
			return "", err
		}
		if pr.Head != req.Head {
			return "", &PullRefError{PR: req.PR, Head: req.Head, Ref: headRef, Got: pr.Head}
		}
		if pr.MergeHead != req.Head {
			return "", &PullRefError{PR: req.PR, Head: req.Head, Ref: fmt.Sprintf("refs/pull/%d/merge", req.PR), Got: pr.MergeHead}
		}
		return pr.Merge, nil
	}
	if err := m.Fetch(ctx, "+"+headRef+":"+headRef); err != nil {
		return "", err
	}
	sha, err := gitOut(ctx, m.Dir, "rev-parse", "--verify", headRef+"^{commit}")
	if err != nil {
		return "", err
	}
	if sha != req.Head {
		return "", &PullRefError{PR: req.PR, Head: req.Head, Ref: headRef, Got: sha}
	}
	return sha, nil
}

// PullRefError means GitHub's refs of a pull request do not show the requested
// head: the head moved on, or the merge ref is not rebuilt for it yet. Nothing
// is tested, so no result is posted for the head. It is an infrastructure
// failure, not a test failure.
type PullRefError struct {
	PR   int
	Head string // the requested head
	Ref  string // the ref that disagrees
	Got  string // the commit Ref points at (for the merge ref, the head it merges)
}

// MergeRef reports whether the merge ref is behind (the head itself is current).
func (e *PullRefError) MergeRef() bool { return strings.HasSuffix(e.Ref, "/merge") }

func (e *PullRefError) Error() string {
	if e.MergeRef() {
		return fmt.Sprintf("merge ref not ready: %s merges %s, not the requested head %s", e.Ref, short(e.Got), short(e.Head))
	}
	return fmt.Sprintf("head moved: %s is at %s, not the requested head %s", e.Ref, short(e.Got), short(e.Head))
}

// Infrastructure marks the error as not caused by the code under test.
func (e *PullRefError) Infrastructure() bool { return true }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "nothing"
	}
	return sha
}

func (r *Runner) start(j *job, batch string) {
	r.ctxOnce.Do(func() {
		r.baseCtx = r.Context
		if r.baseCtx == nil {
			r.baseCtx = context.Background()
		}
	})
	ctx, cancel := context.WithCancelCause(r.baseCtx)
	j.cancel = cancel
	r.mu.Lock()
	if r.active == nil {
		r.active = map[string]*job{}
		r.batch = map[*job]string{}
	}
	r.batch[j] = batch
	if j.req.PR > 0 {
		j.key = fmt.Sprintf("%s#%d %s", j.req.Repo, j.req.PR, j.name)
		if prev := r.active[j.key]; prev != nil {
			prev.cancel(errSuperseded)
		}
		r.active[j.key] = j
	}
	r.active[j.id] = j
	r.mu.Unlock()
	r.waiting.Add(1)
	j.queued = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.active, j.id)
			if r.active[j.key] == j {
				delete(r.active, j.key)
			}
			r.mu.Unlock()
			cancel(nil)
		}()
		j.run(ctx)
		r.checkEnded(j)
	}()
}

// workflowLabel is the workflow's name as it shows on GitHub; a workflow
// without a name is known by its file, without directory and extension.
func workflowLabel(name string) string {
	if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
		return name
	}
	b := path.Base(filepath.ToSlash(name))
	return strings.TrimSuffix(strings.TrimSuffix(b, ".yml"), ".yaml")
}

func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return "c" + hex.EncodeToString(b[:])
}

// headInfo is the `heads` of a check run: the commit statuses attach to, the
// commit tested and what decides whether a result can be reused.
type headInfo struct {
	forge.Head
	Merge     string `json:"merge,omitempty"`
	Tree      string `json:"tree"`
	Toolchain string `json:"toolchain,omitempty"`
	Reuse     string `json:"reuse,omitempty"`
}

func (h headInfo) json() json.RawMessage {
	b, _ := json.Marshal([]headInfo{h})
	return b
}

func parseHead(raw json.RawMessage) (headInfo, bool) {
	var hs []headInfo
	if json.Unmarshal(raw, &hs) != nil || len(hs) != 1 {
		return headInfo{}, false
	}
	return hs[0], true
}

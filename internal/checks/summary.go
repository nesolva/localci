package checks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

// summarySuite is the status context (`localci/checks`) that sums up every check
// of a pull request head: pending while one is queued or running, then the worst
// result. It also says that GitHub's merge ref never got ready for a head. No
// suite may take this name.
//
// What it needs beside the store is kept in memory, so after a server restart a
// summary whose checks never ended is not reposted: the next retry or head
// posts it again.
const summarySuite = "checks"

// headSummary is what the summary of one head commit needs beside the store. mu
// is held while a Submit queues checks and while a summary is computed and
// posted: none counts a half-queued submit, and an older count never lands after
// a newer one.
type headSummary struct {
	mu sync.Mutex
	// planned names the checks of the latest Submit. One with no run still keeps
	// the summary from turning green.
	planned []string
}

func (r *Runner) headSummary(repo, sha string) *headSummary {
	s, _ := r.summaries.LoadOrStore(repo+"@"+sha, &headSummary{})
	return s.(*headSummary)
}

func pullKey(req Request) string { return fmt.Sprintf("%s#%d", req.Repo, req.PR) }

// checkEnded sums up the head of a check again once it has ended.
func (r *Runner) checkEnded(j *job) {
	if j.req.PR == 0 || r.Forge == nil {
		return
	}
	s := r.headSummary(j.req.Repo, j.req.Head)
	s.mu.Lock()
	defer s.mu.Unlock()
	r.postSummary(j.req, s.planned, nil)
}

// postSummary posts the summary of req's head, unless the pull request moved to
// a newer head, which gets its own. starting names checks about to be queued.
// The caller holds the head's lock. Like a check's own status it is best effort:
// one that cannot be computed or posted leaves the previous one standing.
func (r *Runner) postSummary(req Request, planned, starting []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := r.summary(ctx, req.Repo, req.Head, planned, starting)
	if err != nil {
		return
	}
	// Read after the store: if it showed a check superseded by a newer head,
	// that head is already the newest one here.
	if newest, _ := r.newest.Load(pullKey(req)); newest != req.Head {
		return
	}
	_ = r.Forge.SetStatus(ctx, req.Repo, req.Head, st)
}

// How a check counts in the summary, in the words and the order of its
// description.
const (
	checkPassed    = "passed"
	checkFailed    = "failed"
	checkErrored   = "errored"
	checkCancelled = "cancelled"
	checkNotRun    = "not run"
	checkRunning   = "running"
)

func verdict(run store.Run) string {
	switch {
	case !store.Terminal(run.State):
		return checkRunning
	case run.State == store.StatePassed || run.State == store.StateReused:
		return checkPassed
	case run.State == store.StateCancelled:
		return checkCancelled
	// The reasons whose check status is an error: nothing says the code is wrong.
	case strings.HasPrefix(run.Reason, "infrastructure: ") || strings.HasPrefix(run.Reason, "unsupported: "):
		return checkErrored
	}
	return checkFailed
}

// summary sums up the latest run of every check on the pull request head sha,
// whichever Submit started it.
func (r *Runner) summary(ctx context.Context, repo, sha string, planned, starting []string) (forge.Status, error) {
	runs, err := r.Store.Runs(ctx, 0)
	if err != nil {
		return forge.Status{}, err
	}
	checks := map[string]string{} // name -> verdict
	for _, name := range planned {
		checks[name] = checkNotRun
	}
	seen := map[string]bool{}
	for _, run := range runs { // newest first
		if run.Kind != store.KindCheck || seen[run.Suite] {
			continue
		}
		// A local run (no pull request) can be at the head commit too, from a
		// clean checkout: it reports nothing there, so it does not count.
		if h, ok := parseHead(run.Heads); ok && h.PR > 0 && h.Repo == repo && h.SHA == sha {
			seen[run.Suite] = true
			checks[run.Suite] = verdict(run)
		}
	}
	for _, name := range starting {
		checks[name] = checkRunning
	}
	count := map[string]int{}
	for _, v := range checks {
		count[v]++
	}

	st := forge.Status{Suite: summarySuite, State: forge.StateSuccess}
	if n := count[checkRunning]; n > 0 {
		noun := "checks"
		if n == 1 {
			noun = "check"
		}
		st.State, st.Description = forge.StatePending, fmt.Sprintf("%d %s running", n, noun)
		return st, nil
	}
	if len(checks) == 0 {
		st.Description = "no checks apply"
		return st, nil
	}
	var parts []string
	for _, v := range []string{checkPassed, checkFailed, checkErrored, checkCancelled, checkNotRun} {
		if n := count[v]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, v))
		}
	}
	st.Description = strings.Join(parts, ", ")
	switch {
	case count[checkFailed] > 0:
		st.State = forge.StateFailure
	case count[checkPassed] < len(checks):
		st.State = forge.StateError
	}
	return st, nil
}

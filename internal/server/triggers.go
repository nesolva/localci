package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Dispatcher turns pull request triggers into runs: the checks of every watched repository, and the suites whose
// trigger matches. Checks post their own statuses; the dispatcher posts the suites'.
type Dispatcher struct {
	Forge  forge.Forge
	Suites *suite.Runner
	Checks *checks.Runner
	Store  *store.Store
	Bus    *event.Bus
	Config func() *config.Config // the effective configuration, read per trigger
	// URL is the dashboard page of a run, linked from statuses.
	URL func(run string) string
	Log *log.Logger
}

// Handle starts what a trigger asks for. It returns once everything is queued. When GitHub's pull request refs are
// still behind the head, the checks did not start and it returns an error wrapping forge.ErrNotReady: the watcher
// emits the same head again on its next poll (t.Attempt > 1), which runs the checks only, the suites having been
// queued by the first attempt.
func (d *Dispatcher) Handle(ctx context.Context, t forge.Trigger) error {
	cfg := d.Config()
	var notReady error
	if d.Checks != nil && t.Kind == forge.KindPush && checksOn(cfg, t.Repo) {
		_, err := d.Checks.Submit(ctx, checks.Request{Repo: t.Repo, PR: t.PR, Base: t.Base, Head: t.Head, HeadBranch: t.Branch,
			Trigger: "push", RequestedBy: t.Author, LastAttempt: t.LastAttempt})
		var mc *exec.MergeConflictError
		var pe *checks.PullRefError
		switch {
		case errors.As(err, &mc):
			d.printf("%s#%d does not merge into %s: checks not run", t.Repo, t.PR, mc.Base)
		case errors.As(err, &pe) && !t.LastAttempt:
			notReady = fmt.Errorf("checks for %s#%d: %w: %w", t.Repo, t.PR, forge.ErrNotReady, err)
		case err != nil:
			d.printf("checks for %s#%d: %v", t.Repo, t.PR, err)
		}
	}
	if d.Suites == nil || t.Attempt > 1 {
		return notReady
	}
	for _, name := range d.Suites.Names() {
		sd, _ := d.Suites.Suite(name)
		if !slices.Contains(sd.Repos, t.Repo) || !triggers(sd, t) {
			continue
		}
		heads, err := d.Forge.ResolveHeads(ctx, sd.Repos, t)
		if err != nil {
			d.printf("suite %s for %s#%d: %v", name, t.Repo, t.PR, err)
			continue
		}
		req := suite.Request{Suite: name, Trigger: string(t.Kind), RequestedBy: t.Author, Fork: t.Fork}
		// A push runs what the diff can affect and reports it as localci/<suite> (affected); the full suite, the
		// one branch protection requires, runs on the label. The watcher already holds fork heads back until
		// their approval, for pushes as for labels.
		if t.Kind == forge.KindPush {
			req.Affected = &suite.AffectedRequest{Base: t.Base}
		}
		for _, h := range heads {
			req.Heads = append(req.Heads, suite.Head{Head: h})
		}
		// Read the cursor before Submit starts the job: a run that ends fast must not slip between the two.
		since, _ := d.Store.LastSeq(ctx)
		id, err := d.Suites.Submit(req)
		if err != nil {
			d.printf("suite %s for %s#%d: %v", name, t.Repo, t.PR, err)
			continue
		}
		go d.report(ctx, id, suite.StatusName(name, req.Affected != nil), heads, since)
	}
	return notReady
}

func checksOn(c *config.Config, repo string) bool {
	for _, r := range c.Repos {
		if r.Name == repo {
			return r.Checks
		}
	}
	return false
}

func triggers(s *suite.Suite, t forge.Trigger) bool {
	switch t.Kind {
	case forge.KindPush:
		return s.Trigger.Push
	case forge.KindLabel:
		return s.Trigger.Label != "" && s.Trigger.Label == t.Label
	}
	return false
}

// report posts the suite's status on every pull request head of the run as it queues, starts and finishes.
// since is the event cursor taken before the run was submitted, so every event of the run comes after it.
func (d *Dispatcher) report(ctx context.Context, id, name string, heads []forge.Head, since int64) {
	post := func(st forge.Status) {
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := forge.SetStatuses(sctx, d.Forge, heads, st); err != nil {
			d.printf("status of run %s: %v", id, err)
		}
	}
	url := d.URL(id)
	// The runner does not say where a run stands in line, so the description does not claim a number.
	post(forge.Status{Suite: name, State: forge.StatePending, TargetURL: url, Description: "queued"})
	// The subscription lives as long as this report, not as long as serve.
	sub, stop := context.WithCancel(ctx)
	defer stop()
	evs := event.Subscribe(sub, d.Store, d.Bus, max(since-1, 0))
	for e := range evs {
		if e.Run != id || e.Kind != "run.state" {
			continue
		}
		var data struct{ State string }
		if json.Unmarshal(e.Data, &data) != nil {
			continue
		}
		switch {
		case data.State == store.StateRunning:
			c, _ := d.Store.TestCounts(ctx, id)
			total := c.Total
			if p, ok, _ := d.Store.FirstEvent(ctx, id, "run.planned"); ok {
				var pl struct{ Tests int }
				if json.Unmarshal(p.Data, &pl) == nil {
					total = max(total, pl.Tests)
				}
			}
			post(forge.Running(name, c.Passed+c.Failed, total, time.UnixMilli(e.TS), url))
		case store.Terminal(data.State):
			d.final(ctx, id, name, data.State, url, post)
			return
		}
	}
}

func (d *Dispatcher) final(ctx context.Context, id, name, state, url string, post func(forge.Status)) {
	run, err := d.Store.Run(ctx, id)
	if err != nil {
		return
	}
	c, _ := d.Store.TestCounts(ctx, id)
	took := time.Duration(max(run.Finished-run.Started, 0)) * time.Millisecond
	switch state {
	case store.StatePassed, store.StateReused:
		post(forge.Passed(name, c.Passed, took, url))
	case store.StateFailed:
		if run.Infra != "" { // the stack or the tenant flow broke: nothing says the tests are wrong
			post(forge.Status{Suite: name, State: forge.StateError, TargetURL: url, Description: "infrastructure: " + run.Infra})
			return
		}
		if c.Failed == 0 { // the run broke before or around its tests: that is an error, not a failing test
			post(forge.Status{Suite: name, State: forge.StateError, TargetURL: url, Description: "run failed: " + run.Reason})
			return
		}
		first := ""
		if fs, _ := d.Store.Failures(ctx, id); len(fs) > 0 {
			first = fs[0].Title
		}
		post(forge.Failed(name, c.Failed, first, url))
	case store.StateCancelled:
		// A superseded run is replaced by a newer one that reports for itself; any other cancellation clears the
		// pending status so the pull request does not wait for ever.
		if run.Reason != "superseded" {
			post(forge.Status{Suite: name, State: forge.StateError, TargetURL: url, Description: "cancelled: " + run.Reason})
		}
	}
}

func (d *Dispatcher) printf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

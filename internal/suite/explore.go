package suite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
)

// ExploreTrigger is the trigger an exploratory run carries.
const ExploreTrigger = "explore"

// ExploreRequest turns a request into an exploratory run: the suite's repositories at their default branches, one
// isolated shard (a database cloned from the template and the stack), and, instead of tests, Run. Nothing is
// learned from it and no status is posted.
type ExploreRequest struct {
	// BaseURL is where the app listens, a template over the shard's ports ({{port "web"}}).
	BaseURL string
	// Run is the exploration; it ends when it returns or when ctx is done (the run was cancelled).
	Run func(ctx context.Context, env ExploreEnv) (note string, err error)
}

// ExploreEnv is what an exploration gets from its shard.
type ExploreEnv struct {
	Run, Suite string
	Dir        string   // the test repository's workspace
	OutDir     string   // where the exploration writes screenshots and drafts: <log dir>/explore.out
	LogPath    string   // where the agent's output goes
	Env        []string // the shard's environment (LOCALCI_*, and the suite's test environment)
	BaseURL    string
	Emit       func(kind string, data any)
}

// exploreRun is the run of an exploratory request: checkout, prepare, database template, one shard, Run, teardown.
func (ex *execution) exploreRun() (string, error) {
	s, d := ex.suite, ex.r.d
	if err := os.MkdirAll(ex.logDir, 0o755); err != nil {
		return "", err
	}
	per := ex.shardCost()
	release, err := d.Sched.Admit(ex.ctx, sched.Cost{CPUs: per.CPUs, Memory: per.Memory})
	if err != nil {
		return "", err
	}
	defer release()
	ex.setState(store.StatePreparing)
	if err := ex.checkout(); err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	if err := ex.prepare(); err != nil {
		return "", err
	}
	if err := ex.setupDB(); err != nil {
		return "", err
	}
	ex.setState(store.StateRunning)

	log := filepath.Join(ex.logDir, "explore.log")
	ex.putShard(1, store.StateRunning, log, sched.Cost{}, 0)
	e, err := ex.openEnv(ex.ctx, 1, nil)
	if err != nil {
		ex.putShard(1, store.StateFailed, log, sched.Cost{}, 0)
		return "", fmt.Errorf("shard 1: %w", err)
	}
	defer e.close()
	base, err := e.templ().expand(ex.j.req.Explore.BaseURL)
	if err != nil {
		ex.putShard(1, store.StateFailed, log, sched.Cost{}, 0)
		return "", fmt.Errorf("explore.base_url: %w", err)
	}
	user, err := e.userEnv(s.Test.Env)
	if err != nil {
		ex.putShard(1, store.StateFailed, log, sched.Cost{}, 0)
		return "", err
	}
	env := ExploreEnv{Run: ex.id, Suite: s.Name, Dir: ex.dirs[s.Test.Repo], OutDir: filepath.Join(ex.logDir, "explore.out"),
		LogPath: filepath.Join(ex.logDir, "agent.log"), Env: append(user, e.coreEnv()...), BaseURL: base,
		Emit: func(kind string, data any) { ex.emit(1, kind, "", "", data) }}
	note, err := ex.j.req.Explore.Run(ex.ctx, env)
	e.close()
	state := store.StatePassed
	if err != nil {
		state = store.StateFailed
	}
	ex.putShard(1, state, log, sched.Cost{}, 0)
	return note, err
}

// executeExplore ends an exploratory run. It never says "failed" for what the agent found, only for what stopped
// the exploration from happening: the stack, the checkout, the agent command.
func (r *Runner) executeExplore(ex *execution) {
	note, err := ex.exploreRun()
	state, reason := store.StatePassed, note
	switch {
	case ex.j.ctx.Err() != nil:
		state, reason = store.StateCancelled, "cancelled"
		var sc stopCause
		if c := context.Cause(ex.j.ctx); errors.As(c, &sc) {
			reason = string(sc)
		}
	case ex.infraReason() != "":
		state, reason = store.StateFailed, "infrastructure: "+ex.infraReason()
	case err != nil:
		state, reason = store.StateFailed, trim(report.Reason(err.Error()), 600)
	}
	r.finishRun(ex.id, state, strings.TrimSpace(reason))
}

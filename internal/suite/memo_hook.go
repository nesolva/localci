package suite

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/nesolva/localci/internal/memo"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// Memoized execution (T-18). Tests that begin with the same flows share that prefix: the planner (internal/memo)
// hands the first test under each node of the prefix tree to one shard, which builds and checkpoints it, and the
// tests under it to the shards that ask later, which restore. Prefixes are learned from the flow events of runs
// that may teach (learns), so a wrong prediction only means a test runs flows the plan did not expect.
//
// It plans only under the isolated level: shared-db shards are chosen by the tables their tests write and a
// tenant shard owns one tenant, so tests may not move between shards. There the per-test checkpoints of the flow
// service still apply, and database checkpoints are not restored (see the isolation contract).

// memoOn is false only when the suite says `memo: false`.
func memoOn(s *Suite) bool {
	if s.Raw == nil || s.Raw.Kind != yaml.MappingNode {
		return true
	}
	for i := 0; i+1 < len(s.Raw.Content); i += 2 {
		if s.Raw.Content[i].Value == "memo" {
			var on bool
			if err := s.Raw.Content[i+1].Decode(&on); err == nil {
				return on
			}
		}
	}
	return true
}

// memoStart prepares learning for the run and, when earlier runs left usable prefixes, the plan.
func (ex *execution) memoStart(ids []shard.TestID) {
	if !memoOn(ex.suite) || ex.iso != nil {
		return
	}
	ex.mu.Lock()
	ex.rec = memo.NewRecorder()
	ex.mu.Unlock()
	if len(ex.layout.Shards) < 2 || ex.localShards < len(ex.layout.Shards) {
		// With shards on workers the static layout stays: the planner hands batches to every shard of the run, and
		// a worker's stack holds no checkpoint of this machine's database. What the run learns is still recorded.
		return
	}
	prefixes, err := memo.Load(ex.state)
	if err != nil {
		ex.emit(0, "memo.unreadable", "", "", map[string]string{"error": trim(err.Error(), 200)})
		return
	}
	var parallel []string
	for _, tids := range ex.layout.Shards {
		for _, id := range tids {
			parallel = append(parallel, string(id))
		}
	}
	p := memo.NewPlanner(prefixes, parallel, len(ex.layout.Shards))
	if !p.Shared() {
		return
	}
	ex.mu.Lock()
	ex.plan = p
	ex.mu.Unlock()
	ex.emit(0, "memo.planned", "", "", map[string]any{"tests": len(parallel), "shards": len(ex.layout.Shards)})
}

// saveMemo keeps what this run learned. Only runs that learn call it, so a fork never changes the prefix tree.
func (ex *execution) saveMemo() {
	ex.mu.Lock()
	rec := ex.rec
	ex.mu.Unlock()
	if rec == nil {
		return
	}
	if err := memo.Save(ex.state, rec.Learned()); err != nil {
		ex.emit(0, "memo.unsaved", "", "", map[string]string{"error": trim(err.Error(), 200)})
	}
}

func (ex *execution) memoState() (*memo.Recorder, *memo.Planner) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	return ex.rec, ex.plan
}

// runMemoShard is runShard with its work handed out by the planner, batch after batch, on one stack and database.
func (ex *execution) runMemoShard(n int, plan *memo.Planner) error {
	log := filepath.Join(ex.logDir, fmt.Sprintf("shard-%d.log", n))
	ex.putShard(n, store.StateRunning, log, sched.Cost{}, 0)
	e, err := ex.openEnv(ex.ctx, n, nil)
	if err != nil {
		plan.Release(n)
		ex.putShard(n, store.StateFailed, log, sched.Cost{}, 0)
		return fmt.Errorf("shard %d: %w", n, err)
	}
	defer e.close()
	var runErr error
	for k := 1; runErr == nil; k++ {
		w, err := plan.Next(ex.ctx, n)
		if err != nil || w.Done {
			break
		}
		ex.mu.Lock()
		for _, id := range w.Tests {
			ex.envOf[id] = e
		}
		ex.mu.Unlock()
		ids := make([]shard.TestID, len(w.Tests))
		for i, id := range w.Tests {
			ids[i] = shard.TestID(id)
		}
		tag := ""
		if k > 1 {
			tag = ".m" + strconv.Itoa(k)
		}
		inv, err := e.invoke(ex.ctx, ids, modeParallel, tag)
		switch {
		case err != nil:
			runErr = err
		case inv.crashed:
			runErr = errors.New(crashReason(n, inv.res))
		}
	}
	plan.Release(n)
	e.close()
	cost, cpu := e.cost()
	state := store.StatePassed
	if runErr != nil || ex.laneFailed(n) {
		state = store.StateFailed
	}
	ex.putShard(n, state, log, cost, cpu)
	return runErr
}

// memoNext answers POST /runs/<run>/hooks/memo/next?shard=<n>: the next batch for a shard that works from the
// run's plan (a worker on another machine, T-19; local shards call the planner directly). Handing out work is a
// side effect, so the call is a POST with the run's hook token, which the server checks before any hook area.
func (r *Runner) memoNext(w http.ResponseWriter, req *http.Request, hr HookRun, action string) {
	if action != "next" {
		http.Error(w, "use /hooks/memo/next?shard=<n>", http.StatusNotFound)
		return
	}
	n, err := strconv.Atoi(req.URL.Query().Get("shard"))
	if err != nil || n < 1 {
		http.Error(w, "shard must be a number from 1", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	ex := r.live[hr.Run]
	r.mu.Unlock()
	work := memo.Work{Done: true} // no plan: nothing to hand out
	if ex != nil {
		if _, plan := ex.memoState(); plan != nil {
			work = plan.Try(n)
		}
	}
	if work.Tests == nil {
		work.Tests = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(work)
}

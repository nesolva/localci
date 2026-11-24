package suite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// ETA is how long a run still needs: the planned makespan of its shards plus its serial group, from the
// medians of earlier runs, minus the time it has been running. ok is false until the run has a plan, and once
// it is finished.
func (r *Runner) ETA(ctx context.Context, id string) (eta time.Duration, ok bool, err error) {
	run, err := r.d.Store.Run(ctx, id)
	if err != nil {
		return 0, false, err
	}
	if store.Terminal(run.State) {
		return 0, false, nil
	}
	evs, err := r.d.Store.RunEvents(ctx, id)
	if err != nil {
		return 0, false, err
	}
	for _, e := range evs {
		if e.Kind != "run.planned" {
			continue
		}
		var p struct {
			Makespan int64 `json:"makespan_ms"`
			Alone    int64 `json:"alone_ms"`
		}
		if json.Unmarshal(e.Data, &p) != nil {
			return 0, false, nil
		}
		total := time.Duration(p.Makespan+p.Alone) * time.Millisecond
		if run.Started == 0 {
			return total, true, nil // planned, not yet started: all of it is ahead
		}
		return max(total-time.Since(time.UnixMilli(run.Started)), 0), true, nil
	}
	return 0, false, nil
}

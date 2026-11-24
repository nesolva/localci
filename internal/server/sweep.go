package server

import (
	"context"
	"errors"
	"log"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// sweepLeftovers removes what a hard kill left on the machine: half-built template and checkpoint databases, the
// databases of finished runs, and the containers of finished runs. Runs the previous process left unfinished must
// already be cancelled, so their leftovers count as finished. Nothing here is fatal: a server or Docker that
// does not answer is logged and the next start tries again. It runs beside the server, which need not wait for it.
func sweepLeftovers(ctx context.Context, st *store.Store, suites []*suite.Suite, dataDir string, lg *log.Logger) {
	runs, err := st.Runs(ctx, 0)
	if err != nil {
		lg.Printf("sweep: list runs: %v", err)
		return
	}
	finished := map[string]bool{}
	refs := make([]shard.RunRef, 0, len(runs))
	for _, r := range runs {
		finished[r.ID] = store.Terminal(r.State)
		refs = append(refs, shard.RunRef{Suite: r.Suite, ID: r.ID, Finished: finished[r.ID]})
	}

	seen := map[[3]string]bool{}
	for _, s := range suites {
		d := s.Database
		if d == nil || d.Driver != "postgres" {
			continue
		}
		if k := [3]string{d.URL, d.Container, d.User}; seen[k] {
			continue
		} else {
			seen[k] = true
		}
		pg := &shard.Postgres{URL: d.URL, Container: d.Container, User: d.User, LockDir: shard.LockDir(dataDir)}
		dropped, err := shard.SweepOrphans(ctx, pg, shard.InstanceID(dataDir), refs)
		if len(dropped) > 0 {
			lg.Printf("sweep: dropped %d leftover databases", len(dropped))
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			lg.Printf("sweep: databases of suite %s: %v", s.Name, err)
		}
	}

	if n := exec.SweepRunContainers(ctx, func(run string) bool { return finished[run] }); n > 0 {
		lg.Printf("sweep: removed %d leftover containers", n)
	}
}

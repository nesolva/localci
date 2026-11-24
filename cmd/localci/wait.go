package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

// pollEvery is how often wait asks the server; tests shorten it.
var pollEvery = time.Second

func init() {
	register("wait", "wait for runs to finish and say how they ended", true,
		agent("wait", "<id…> [--timeout 30m]", func(fs *flag.FlagSet) {
			fs.Duration("timeout", defaultWait, "give up after this long (exit 4)")
		}, func(e *env, ids []string) int {
			if len(ids) == 0 {
				fmt.Fprintln(e.err, "usage: localci wait <id…>")
				return exitUsage
			}
			d, _ := time.ParseDuration(e.fs.Lookup("timeout").Value.String())
			return waitFor(e, ids, d)
		}))
}

// waitFor blocks until every run is finished, printing each one's line as it ends, and returns the exit code of
// the worst outcome.
func waitFor(e *env, ids []string, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	pending := append([]string(nil), ids...)
	var codes []int
	for len(pending) > 0 {
		var still []string
		for _, id := range pending {
			var r server.RunDetail
			if err := e.c.get("/api/runs/"+id, &r); err != nil {
				return e.fail(err)
			}
			if !store.Terminal(r.State) {
				still = append(still, id)
				continue
			}
			if e.json {
				e.emit("run", r.Run)
			} else {
				fmt.Fprintln(e.out, runLine(r.Run))
			}
			codes = append(codes, exitOf(r.Run))
		}
		pending = still
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for _, id := range pending {
				if e.json {
					e.emit("timeout", struct {
						Run string `json:"run"`
					}{id})
				} else {
					fmt.Fprintf(e.out, "#%s still not finished after %s · localci wait %s\n", id, fmtDur(timeout), id)
				}
			}
			return exitTimeout
		}
		time.Sleep(pollEvery)
	}
	return worst(codes...)
}

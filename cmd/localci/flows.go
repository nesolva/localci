package main

import (
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/flow"
)

func init() {
	register("flows", "reusable flows the runs called: inputs, outputs, use, time, checkpoint hit rate", true,
		agent("flows", "[--all]", func(fs *flag.FlagSet) {
			fs.Bool("all", false, "every flow, not the first 10")
		}, cmdFlows))
}

func cmdFlows(e *env, _ []string) int {
	var l struct {
		Flows []flow.Summary `json:"flows"`
	}
	if err := e.c.get("/api/flows", &l); err != nil {
		return e.fail(err)
	}
	shown := l.Flows
	if !flagOn(e, "all") && len(shown) > listLines {
		shown = shown[:listLines]
	}
	for _, f := range shown {
		if e.json {
			e.emit("flow", f)
		} else {
			fmt.Fprintln(e.out, flowLine(f))
		}
	}
	if e.json {
		return exitOK
	}
	if len(l.Flows) == 0 {
		fmt.Fprintln(e.out, "no flow has run yet (declared ones: npx --no-install --offline localci-playwright flows)")
	}
	if n := len(l.Flows) - len(shown); n > 0 {
		fmt.Fprintf(e.out, "%d more: localci flows --all\n", n)
	}
	return exitOK
}

// flowLine is one flow: name, what it takes and returns, how much it is used, how long it takes and how often a
// checkpoint spared the run.
func flowLine(f flow.Summary) string {
	parts := []string{f.Name, "in: " + names(f.Inputs), "out: " + names(f.Outputs), plural(f.Tests, "test", "tests")}
	if f.RunMS > 0 {
		parts = append(parts, "run "+fmtDur(time.Duration(f.RunMS)*time.Millisecond))
	}
	if f.RestoreMS > 0 {
		parts = append(parts, "restore "+fmtDur(time.Duration(f.RestoreMS)*time.Millisecond))
	}
	if f.Checkpointed > 0 {
		parts = append(parts, fmt.Sprintf("hit %d%%", int(f.HitRate*100+0.5)))
	}
	return strings.Join(parts, "  ")
}

func names(m map[string]any) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

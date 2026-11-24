package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/nesolva/localci/internal/explore"
)

func init() {
	register("findings", "what the nightly exploratory agent found: title, goal, run; with steps and files on request", true,
		agent("findings", "[--all] [--full] [--since id|12h] [--run id]", func(fs *flag.FlagSet) {
			fs.Bool("all", false, "every finding, not the first 10")
			fs.Bool("full", false, "also print the steps, the screenshot and the draft test of each")
			fs.String("since", "", "only findings after this number or found within this long (12h); ends with `cursor <id>`")
			fs.String("run", "", "only the findings of this exploratory run")
		}, cmdFindings))
}

func cmdFindings(e *env, _ []string) int {
	since := e.fs.Lookup("since").Value.String()
	q := url.Values{"limit": {strconv.Itoa(listLines + 1)}}
	if flagOn(e, "all") {
		q.Set("limit", "500")
	}
	if since != "" {
		q.Set("since", since)
	}
	if run := e.fs.Lookup("run").Value.String(); run != "" {
		q.Set("run", run)
	}
	var l explore.List
	if err := e.c.get("/api/findings?"+q.Encode(), &l); err != nil {
		return e.fail(err)
	}
	shown := l.Findings
	if !flagOn(e, "all") && len(shown) > listLines {
		shown = shown[:listLines]
	}
	for _, f := range shown {
		if e.json {
			e.emit("finding", f)
			continue
		}
		fmt.Fprintf(e.out, "#%d %s  %s  (run %s, %s)", f.ID, f.Severity, printable(f.Title), f.Run, time.UnixMilli(f.TS).Format("Jan 2 15:04"))
		if f.Goal != "" {
			fmt.Fprintf(e.out, "  goal: %s", printable(f.Goal))
		}
		fmt.Fprintln(e.out)
		if flagOn(e, "full") {
			for i, s := range f.Steps {
				fmt.Fprintf(e.out, "    %d. %s\n", i+1, printable(s))
			}
			if f.Screenshot != "" {
				fmt.Fprintf(e.out, "    screenshot: %s\n", f.Screenshot)
			}
			if f.Test != "" {
				fmt.Fprintf(e.out, "    draft test: %s\n", f.Test)
			}
		}
	}
	if !e.json {
		if len(l.Findings) == 0 {
			fmt.Fprintln(e.out, "no findings (an exploratory run is started with `localci explore`)")
		}
		if n := l.Total - len(shown); n > 0 {
			fmt.Fprintf(e.out, "%d more: localci findings --all\n", n)
		}
	}
	if since != "" {
		e.cursorLine(l.Cursor)
	}
	return exitOK
}

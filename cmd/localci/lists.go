package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"

	"github.com/nesolva/localci/internal/server"
)

func init() {
	register("runs", "recent runs, newest first", true,
		agent("runs", "[--all] [--since seq|30m] [--flaky]", func(fs *flag.FlagSet) {
			fs.Bool("all", false, "every run, not the first 10")
			fs.Bool("flaky", false, "instead of runs: the tests that failed and then passed, with how many runs in the last 30 days")
			fs.String("since", "", "only runs touched since this event number or this long ago (30m); ends with `cursor <seq>`")
		}, cmdRuns))
	register("queue", "what is running or waiting", true,
		agent("queue", "[--all]", func(fs *flag.FlagSet) {
			fs.Bool("all", false, "every run, not the first 10")
		}, cmdQueue))
	register("status", "one screen: what is running, what is waiting, how the last runs ended", true,
		agent("status", "", nil, cmdStatus))
	register("workers", "the machines that run shards and how busy they are; `workers token` lets another machine join, `workers revoke <name>` removes it", true,
		agent("workers", "[list | token [--host h] | revoke <name>]", func(fs *flag.FlagSet) {
			fs.String("host", "", "token: the address the worker machine reaches this one at (shown in the join command)")
		}, cmdWorkers))
	register("eta", "how long a run still needs", true,
		agent("eta", "<id…>", nil, cmdETA))
}

func flagOn(e *env, name string) bool { return e.fs.Lookup(name).Value.String() == "true" }

func cmdRuns(e *env, _ []string) int {
	if flagOn(e, "flaky") {
		return cmdFlaky(e)
	}
	since := e.fs.Lookup("since").Value.String()
	limit := listLines + 1
	if flagOn(e, "all") {
		limit = 500
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if since != "" {
		q.Set("since", since)
	}
	var l server.RunList
	if err := e.c.get("/api/runs?"+q.Encode(), &l); err != nil {
		return e.fail(err)
	}
	shown := l.Runs
	if !flagOn(e, "all") && len(shown) > listLines {
		shown = shown[:listLines]
	}
	for _, r := range shown {
		if e.json {
			e.emit("run", r)
		} else {
			fmt.Fprintln(e.out, listLine(r))
		}
	}
	if len(shown) == 0 && !e.json {
		fmt.Fprintln(e.out, "no runs")
	}
	if n := l.Total - len(shown); n > 0 && !e.json {
		fmt.Fprintf(e.out, "%d more: localci runs --all\n", n)
	}
	if since != "" {
		e.cursorLine(l.Cursor)
	}
	return exitOK
}

// listLine is a run in a list: suites and checks are named so rows can be told apart.
func listLine(r server.Run) string {
	if r.State == "queued" || r.State == "preparing" || r.State == "running" {
		return runLine(r)
	}
	return finishedLine(r, true)
}

func cmdQueue(e *env, _ []string) int {
	var q server.Queue
	if err := e.c.get("/api/queue", &q); err != nil {
		return e.fail(err)
	}
	shown := q.Runs
	if !flagOn(e, "all") && len(shown) > listLines {
		shown = shown[:listLines]
	}
	for _, r := range shown {
		if e.json {
			e.emit("run", r)
		} else {
			fmt.Fprintln(e.out, runLine(r))
		}
	}
	if !e.json {
		if len(q.Runs) == 0 {
			fmt.Fprintln(e.out, "queue empty")
		}
		if n := len(q.Runs) - len(shown); n > 0 {
			fmt.Fprintf(e.out, "%d more: localci queue --all\n", n)
		}
	}
	return exitOK
}

func cmdStatus(e *env, _ []string) int {
	var q server.Queue
	var recent server.RunList
	var w server.Workers
	if err := e.c.get("/api/queue", &q); err != nil {
		return e.fail(err)
	}
	if err := e.c.get("/api/runs?limit=12", &recent); err != nil {
		return e.fail(err)
	}
	if err := e.c.get("/api/workers", &w); err != nil {
		return e.fail(err)
	}
	running, queued := 0, 0
	for _, r := range q.Runs {
		if r.State == "queued" {
			queued++
		} else {
			running++
		}
	}
	var done []server.Run
	for _, r := range recent.Runs {
		if r.State != "queued" && r.State != "preparing" && r.State != "running" {
			done = append(done, r)
		}
	}
	if e.json {
		e.emit("status", struct {
			Running int             `json:"running"`
			Queued  int             `json:"queued"`
			Workers []server.Worker `json:"workers"`
		}{running, queued, w.Workers})
		for _, r := range q.Runs {
			e.emit("run", r)
		}
		for _, r := range done[:min(len(done), 3)] {
			e.emit("run", r)
		}
		return exitOK
	}
	head := fmt.Sprintf("localci up · %d running · %d queued", running, queued)
	if len(w.Workers) > 0 {
		head += fmt.Sprintf(" · %d/%d cpus busy", w.Workers[0].UsedCPUs, w.Workers[0].CPUs)
	}
	fmt.Fprintln(e.out, head)
	active := q.Runs[:min(len(q.Runs), 6)]
	for _, r := range active {
		fmt.Fprintln(e.out, runLine(r))
	}
	if n := len(q.Runs) - len(active); n > 0 {
		fmt.Fprintf(e.out, "%d more: localci queue --all\n", n)
	}
	room := listLines - 1 - len(active)
	if len(q.Runs) > len(active) {
		room--
	}
	for _, r := range done[:min(len(done), max(min(room, 3), 0))] {
		fmt.Fprintln(e.out, listLine(r))
	}
	return exitOK
}

func cmdETA(e *env, ids []string) int {
	if len(ids) == 0 {
		fmt.Fprintln(e.err, "usage: localci eta <id…>")
		return exitUsage
	}
	for _, id := range ids {
		var r server.RunDetail
		if err := e.c.get("/api/runs/"+url.PathEscape(id), &r); err != nil {
			return e.fail(err)
		}
		if e.json {
			e.emit("eta", struct {
				Run   string `json:"run"`
				State string `json:"state"`
				ETAMS int64  `json:"eta_ms"`
			}{r.ID, r.State, r.ETAMS})
			continue
		}
		switch {
		case r.State == "queued" || r.State == "preparing" || r.State == "running":
			if r.ETAMS > 0 {
				fmt.Fprintf(e.out, "#%s %s · %s left\n", r.ID, r.State, fmtETA(r.ETAMS))
			} else {
				fmt.Fprintf(e.out, "#%s %s · no estimate yet\n", r.ID, r.State)
			}
		default:
			fmt.Fprintln(e.out, finishedLine(r.Run, false))
		}
	}
	return exitOK
}

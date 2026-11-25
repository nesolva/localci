package main

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

func init() {
	register("show", "what failed in a run: step, expected vs actual, aria, console, network, files", true,
		agent("show", "<id> [--all] [--full] [--since seq]", func(fs *flag.FlagSet) {
			fs.Bool("all", false, "every failure and shard, not only the first failure and ten shards")
			fs.Bool("full", false, "do not shorten a failure to 25 lines")
			fs.String("since", "", "only failures newer than this event number; ends with `cursor <seq>`")
		}, cmdShow))
}

const (
	failureLines = 25 // per failure, unless --full
	ariaLines    = 12
	listLines    = 10
)

func cmdShow(e *env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(e.err, "usage: localci show <id> [--all] [--full]")
		return exitUsage
	}
	id := args[0]
	all, full := e.fs.Lookup("all").Value.String() == "true", e.fs.Lookup("full").Value.String() == "true"
	since := e.fs.Lookup("since").Value.String()
	q := ""
	if since != "" {
		if n, err := strconv.ParseInt(since, 10, 64); err != nil || n < 0 {
			fmt.Fprintf(e.err, "localci: --since for show is an event number (the cursor of an earlier call), got %q\n", since)
			return exitUsage
		}
		q = "?since=" + url.QueryEscape(since)
	}
	var fl server.FailureList
	if err := e.c.get("/api/runs/"+url.PathEscape(id)+"/failures"+q, &fl); err != nil {
		return e.fail(err)
	}
	// The shards come with the run, not with its failures; a run still being queued has none.
	var detail server.RunDetail
	if since == "" {
		if err := e.c.get("/api/runs/"+url.PathEscape(id), &detail); err != nil {
			return e.fail(err)
		}
	}
	shown := fl.Failures
	if !all && len(shown) > 1 {
		shown = shown[:1]
	}

	if e.json {
		e.emit("run", fl.Run)
		for _, f := range shown {
			e.emit("failure", f)
		}
		for _, sh := range detail.ShardList {
			e.emit("shard", sh)
		}
		if len(fl.Failures) > len(shown) {
			e.emit("more", struct {
				Count int `json:"count"`
			}{len(fl.Failures) - len(shown)})
		}
		if since != "" {
			e.cursorLine(fl.Cursor)
		}
		return exitOK
	}

	if fl.Run.Infra != "" && len(fl.Failures) > 0 { // without failures the run line says it
		fmt.Fprintln(e.out, infraText(fl.Run))
	}
	if len(fl.Failures) == 0 {
		fmt.Fprintln(e.out, runLine(fl.Run))
		if since != "" && fl.Total > 0 {
			fmt.Fprintln(e.out, "no new failures")
		}
	} else if !store.Terminal(fl.Run.State) {
		fmt.Fprintln(e.out, runLine(fl.Run))
	}
	for _, f := range shown {
		for _, l := range failureText(f, full) {
			fmt.Fprintln(e.out, l)
		}
	}
	if n := len(fl.Failures) - len(shown); n > 0 {
		fmt.Fprintf(e.out, "%d more: localci show %s --all\n", n, id)
	}
	shards := detail.ShardList
	if !all && len(shards) > listLines {
		shards = shards[:listLines]
	}
	for _, sh := range shards {
		line := fmt.Sprintf("%s · %s", shardName(sh.Label, sh.N), sh.State)
		if sh.LogPath != "" {
			line += " · " + sh.LogPath
		}
		fmt.Fprintln(e.out, line)
	}
	if n := len(detail.ShardList) - len(shards); n > 0 {
		fmt.Fprintf(e.out, "%d more shards: localci show %s --all\n", n, id)
	}
	if since != "" {
		e.cursorLine(fl.Cursor)
	}
	return exitOK
}

// failureText is the report of one failed test: at most 25 lines unless full. Everything in it but the shard
// comes from the report the code under test wrote, so every such string goes through printable.
func failureText(f server.Failure, full bool) []string {
	cut := func(s string, n int) string {
		if full {
			return strings.Join(strings.Fields(printable(s)), " ")
		}
		return trim(s, n)
	}
	where := f.Location
	if where == "" {
		where = f.File
		if f.Line > 0 {
			where += ":" + strconv.Itoa(f.Line)
		}
	}
	shard := shardName(f.Label, f.Shard)
	first := fmt.Sprintf("✗ %s · %s · %s · %s", printable(f.Title), printable(where), shard, fmtDur(time.Duration(f.MS)*time.Millisecond))
	if f.Class != "" {
		first += " · class: " + printable(f.Class)
	}
	lines := []string{first}

	var step []string
	if f.Step != "" {
		step = append(step, "step: "+cut(f.Step, 120))
	}
	if f.Expected != "" || f.Actual != "" {
		if f.Expected != "" {
			step = append(step, "expected: "+cut(f.Expected, 160))
		}
		if f.Actual != "" {
			step = append(step, "actual: "+cut(f.Actual, 160))
		}
	} else if f.Message != "" {
		first, _, _ := strings.Cut(strings.TrimSpace(f.Message), "\n")
		step = append(step, "error: "+cut(first, 200))
	}
	if len(step) > 0 {
		lines = append(lines, "  "+strings.Join(step, " · "))
	}

	if f.Aria != "" {
		al := strings.Split(strings.TrimRight(f.Aria, "\n"), "\n")
		extra := 0
		if !full && len(al) > ariaLines {
			extra, al = len(al)-ariaLines, al[:ariaLines]
		}
		for i, l := range al { // split first: printable makes a line break a space
			if i == 0 {
				lines = append(lines, "  aria: "+printable(l))
			} else {
				lines = append(lines, "        "+printable(l))
			}
		}
		if extra > 0 {
			lines = append(lines, fmt.Sprintf("        … %d more lines (--full)", extra))
		}
	}

	var net []string
	for _, n := range f.Network {
		net = append(net, fmt.Sprintf("%s %s %d", printable(n.Method), printable(n.URL), n.Status))
	}
	if full {
		for _, c := range f.Console {
			lines = append(lines, "  console: "+cut(c, 0))
		}
		for _, n := range net {
			lines = append(lines, "  network: "+n)
		}
	} else {
		var parts []string
		if len(f.Console) > 0 {
			var cs []string
			for _, c := range f.Console[:min(len(f.Console), 3)] {
				cs = append(cs, trim(c, 160))
			}
			parts = append(parts, "console: "+strings.Join(cs, " | "))
		}
		if len(net) > 0 {
			parts = append(parts, "network: "+strings.Join(net[:min(len(net), 3)], ", "))
		}
		if len(parts) > 0 {
			lines = append(lines, "  "+strings.Join(parts, " · "))
		}
	}

	var files []string
	for _, p := range []string{f.Screenshot, f.Trace} {
		if p != "" {
			files = append(files, printable(p))
		}
	}
	if len(files) > 0 {
		lines = append(lines, "  files: "+strings.Join(files, " "))
	}
	if !full && len(lines) > failureLines {
		lines = lines[:failureLines]
	}
	return lines
}

// shardName is how a shard is called: its label when it has one ("serial"), else its number.
func shardName(label string, n int) string {
	if label != "" {
		return label
	}
	if n == 0 {
		return "serial"
	}
	return "shard " + strconv.Itoa(n)
}

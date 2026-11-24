package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

// fmtDur renders a duration the way a line of output wants it: 8.1s, 41s, 5m12s, 1h02m.
func fmtDur(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m, s := int(d.Minutes()), int(d.Seconds())%60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// fmtETA is a rough time left: ~45s, ~6m, ~1h10m.
func fmtETA(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("~%ds", max(int(d.Seconds()), 1))
	case d < time.Hour:
		return fmt.Sprintf("~%dm", int((d+30*time.Second)/time.Minute))
	}
	return fmt.Sprintf("~%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func short(sha string) string {
	if len(sha) > 6 {
		return sha[:6]
	}
	return sha
}

// headsText names what a run tests: acme/web working tree a1b2c3+, acme/web#12 a1b2c3, acme/web main@a1b2c3.
func headsText(hs []server.Head) string {
	var parts []string
	for _, h := range hs {
		switch {
		case h.Dir != "":
			plus := ""
			if h.Dirty {
				plus = "+"
			}
			parts = append(parts, fmt.Sprintf("%s working tree %s%s", h.Repo, short(h.SHA), plus))
		case h.PR > 0:
			parts = append(parts, fmt.Sprintf("%s#%d %s", h.Repo, h.PR, short(h.SHA)))
		case h.Branch != "" && h.SHA != "":
			parts = append(parts, fmt.Sprintf("%s %s@%s", h.Repo, h.Branch, short(h.SHA)))
		case h.SHA != "":
			parts = append(parts, fmt.Sprintf("%s %s", h.Repo, short(h.SHA)))
		default:
			parts = append(parts, h.Repo)
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// took is how long a finished run ran, 0 when it never started.
func took(r server.Run) time.Duration {
	if r.Started == 0 || r.Finished < r.Started {
		return 0
	}
	return time.Duration(r.Finished-r.Started) * time.Millisecond
}

// runLine is one run on one line. A run that is waiting or running says what it tests and how long it needs; a
// finished one says how it ended and where to look next.
func runLine(r server.Run) string {
	if !store.Terminal(r.State) {
		parts := []string{fmt.Sprintf("#%s %s %s", r.ID, r.State, r.Suite)}
		if h := headsText(r.Heads); h != "" {
			parts = append(parts, h)
		}
		if r.Tests > 0 {
			parts = append(parts, plural(r.Tests, "test", "tests"))
		}
		if r.Shards > 0 {
			parts = append(parts, plural(r.Shards, "shard", "shards"))
		}
		if r.ETAMS > 0 {
			parts = append(parts, fmtETA(r.ETAMS))
		}
		parts = append(parts, "localci wait "+r.ID)
		return strings.Join(parts, " · ")
	}
	return finishedLine(r, r.Kind == store.KindCheck)
}

// finishedLine is `#42 failed 2/128 · 5m12s · localci show 42`; named adds the suite or check, for lists.
func finishedLine(r server.Run, named bool) string {
	head := "#" + r.ID + " " + r.State
	var parts []string
	switch {
	case r.Kind == store.KindSuite && r.Tests > 0 && r.State == store.StateFailed && r.Failed > 0 && r.Infra == "":
		head += fmt.Sprintf(" %d/%d", r.Failed, r.Tests)
	case r.Kind == store.KindSuite && r.Tests > 0 && (r.State == store.StatePassed || r.State == store.StateReused):
		head += fmt.Sprintf(" %d/%d", r.Passed, r.Tests)
	case r.State == store.StateCancelled && r.Reason != "":
		head += " (" + r.Reason + ")"
	}
	parts = append(parts, head)
	if named {
		parts = append(parts, r.Suite)
	}
	if d := took(r); d > 0 {
		parts = append(parts, fmtDur(d))
	}
	switch r.State {
	case store.StateFailed:
		switch {
		case r.Infra != "":
			parts = append(parts, infraText(r), "localci logs "+r.ID)
		case r.Kind == store.KindSuite && r.Failed > 0:
			parts = append(parts, "localci show "+r.ID)
		case r.Reason != "":
			parts = append(parts, trim(r.Reason, 80), "localci logs "+r.ID)
		default:
			parts = append(parts, "localci logs "+r.ID)
		}
	}
	return strings.Join(parts, " · ")
}

// trim cuts s to n runes and keeps it on one line, printable.
func trim(s string, n int) string {
	s = strings.Join(strings.Fields(printable(s)), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// printable makes text from a run's report (written by the code under test) safe for a terminal.
func printable(s string) string { return report.Printable(s) }

// infraText is how a run says the stack or the tenant flow broke.
func infraText(r server.Run) string { return "infrastructure: " + trim(r.Infra, report.MaxReason) }

// exitOf is the exit code a finished run stands for.
func exitOf(r server.Run) int {
	switch r.State {
	case store.StatePassed, store.StateReused:
		return exitOK
	case store.StateFailed:
		if r.Infra != "" {
			return exitInfra
		}
		if r.Kind == store.KindCheck || r.Failed > 0 {
			return exitFailed
		}
		return exitInfra
	}
	return exitInfra
}

// worst picks the exit code that matters most when several runs ended differently.
func worst(codes ...int) int {
	rank := map[int]int{exitOK: 0, exitFailed: 1, exitInfra: 2, exitTimeout: 3}
	out := exitOK
	for _, c := range codes {
		if rank[c] > rank[out] {
			out = c
		}
	}
	return out
}

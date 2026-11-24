package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

func init() {
	register("menubar", "menu bar output for SwiftBar or xbar (see examples/menubar); `menubar retry <id>` reruns a run", false,
		agent("menubar", "[--dashboard url] [--bin path] | retry <id>", func(fs *flag.FlagSet) {
			fs.String("dashboard", defaultDashboard(), "address of the dashboard the menu links to")
			fs.String("bin", "", "path the menu's retry action runs (default: this executable)")
		}, cmdMenubar))
}

func defaultDashboard() string {
	port := os.Getenv("LOCALCI_SERVER_PORT")
	if port == "" {
		port = "7357"
	}
	return "http://127.0.0.1:" + port
}

// recentResults is how many finished runs the menu lists.
const recentResults = 5

func cmdMenubar(e *env, args []string) int {
	if len(args) > 0 {
		if args[0] != "retry" || len(args) != 2 {
			fmt.Fprintln(e.err, "usage: localci menubar [--dashboard url] | retry <id>")
			return exitUsage
		}
		if err := e.c.post("/api/runs/"+url.PathEscape(args[1])+"/retry", nil, nil); err != nil {
			return e.fail(err)
		}
		return exitOK
	}
	bin := e.fs.Lookup("bin").Value.String()
	if bin == "" {
		bin, _ = os.Executable()
	}
	in := menuInput{Now: time.Now(), Dashboard: e.fs.Lookup("dashboard").Value.String(), Bin: bin}
	var q server.Queue
	var recent server.RunList
	// A menu that cannot reach the server still has to draw: it says so instead of failing.
	if err := e.c.get("/api/queue", &q); err != nil {
		in.Offline = true
	} else if err := e.c.get("/api/runs?limit=40", &recent); err != nil {
		in.Offline = true
	}
	in.Active, in.Recent = q.Runs, recent.Runs
	renderMenu(e.out, in)
	return exitOK
}

// menuInput is everything the menu is drawn from, so the drawing is a pure function of it.
type menuInput struct {
	Now       time.Time
	Dashboard string
	Bin       string // what a menu action runs
	Offline   bool
	Active    []server.Run // running or waiting, from the queue
	Recent    []server.Run // newest first
}

// Menu states, in order of how much attention they ask for.
const (
	menuIdle    = "idle"
	menuRunning = "running"
	menuFailed  = "failed"
)

// failedNow reports whether some suite or check is currently red: its newest finished run failed. A later pass
// of the same suite clears it, so the title does not stay red for the next five runs of other suites.
func failedNow(recent []server.Run) bool {
	seen := map[string]bool{}
	for _, r := range recent {
		if !store.Terminal(r.State) || r.State == store.StateCancelled {
			continue
		}
		key := r.Kind + "/" + r.Suite
		if seen[key] {
			continue
		}
		seen[key] = true
		if r.State == store.StateFailed {
			return true
		}
	}
	return false
}

// renderMenu writes SwiftBar/xbar plugin output. The title says what is running and the worst state once; the
// running runs carry the weight below it, finished ones recede unless they failed.
func renderMenu(w io.Writer, in menuInput) {
	if in.Offline {
		fmt.Fprintln(w, "localci off | sfimage=moon.zzz")
		fmt.Fprintln(w, "---")
		fmt.Fprintln(w, "localci is not running")
		fmt.Fprintln(w, "Start it: localci serve, or localci service install | color=gray")
		return
	}
	var running, queued []server.Run
	for _, r := range in.Active {
		if r.State == store.StateQueued {
			queued = append(queued, r)
		} else {
			running = append(running, r)
		}
	}
	failed := failedNow(in.Recent)
	state := menuIdle
	switch {
	case failed:
		state = menuFailed
	case len(running)+len(queued) > 0:
		state = menuRunning
	}

	var title []string
	switch {
	case len(running) > 0:
		title = append(title, fmt.Sprintf("%d running", len(running)))
	case len(queued) > 0:
		title = append(title, fmt.Sprintf("%d queued", len(queued)))
	}
	if failed {
		title = append(title, "failed")
	} else if len(title) == 0 {
		title = append(title, menuIdle)
	}
	icon := map[string]string{menuIdle: "checkmark.circle", menuRunning: "arrow.triangle.2.circlepath", menuFailed: "exclamationmark.triangle.fill"}[state]
	attrs := "sfimage=" + icon
	if state == menuFailed {
		attrs += " color=red"
	}
	fmt.Fprintf(w, "%s | %s\n---\n", strings.Join(title, " · "), attrs)

	for _, r := range running {
		fmt.Fprintf(w, "%s | href=%s\n", activeText(r, in.Now), runURL(in.Dashboard, r.ID))
	}
	for _, r := range queued {
		fmt.Fprintf(w, "%s | color=gray href=%s\n", activeText(r, in.Now), runURL(in.Dashboard, r.ID))
	}
	if len(running)+len(queued) > 0 {
		fmt.Fprintln(w, "---")
	}

	shown := 0
	for _, r := range in.Recent {
		if !store.Terminal(r.State) {
			continue
		}
		if shown == recentResults {
			break
		}
		shown++
		line, bad := resultText(r, in.Now)
		href := runURL(in.Dashboard, r.ID)
		if !bad {
			fmt.Fprintf(w, "%s | color=gray href=%s\n", line, href)
			continue
		}
		// A failure is the one row with something to do: open it or run it again.
		fmt.Fprintf(w, "%s | color=red\n", line)
		fmt.Fprintf(w, "--Open run | href=%s\n", href)
		fmt.Fprintf(w, "--Retry | bash=%s param1=menubar param2=retry param3=%s terminal=false refresh=true\n", quoteParam(in.Bin), r.ID)
	}
	if shown == 0 {
		fmt.Fprintln(w, "No runs yet | color=gray")
	}
	fmt.Fprintln(w, "---")
	fmt.Fprintf(w, "Open dashboard | href=%s\n", in.Dashboard)
}

// label makes a name safe for a menu line: `|` starts the parameters and a line break starts a new menu item, so
// neither can come out of a suite, job or repo name.
func label(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '|':
			return '/'
		case r == '\u0085' || r == '\u2028' || r == '\u2029' || unicode.IsControl(r):
			return ' '
		}
		return r
	}, s)
}

func runURL(dashboard, id string) string { return strings.TrimRight(dashboard, "/") + "/runs/" + id }

// quoteParam keeps a path with spaces as one SwiftBar parameter.
func quoteParam(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// activeText is `e2e #43 · 2m10s · ~4m left` for a running run and `e2e #44 · waiting 12s` for a queued one.
func activeText(r server.Run, now time.Time) string {
	if r.State == store.StateQueued {
		return fmt.Sprintf("%s #%s · waiting %s", label(r.Suite), r.ID, fmtDur(sinceMS(now, r.Created)))
	}
	parts := []string{fmt.Sprintf("%s #%s", label(r.Suite), r.ID)}
	start := r.Started
	if start == 0 {
		start = r.Created
	}
	parts = append(parts, fmtDur(sinceMS(now, start)))
	if r.ETAMS > 0 {
		parts = append(parts, fmtETA(r.ETAMS)+" left")
	}
	return strings.Join(parts, " · ")
}

// resultText is `✓ e2e #42 · 126 tests · 5m12s · 3h ago`; bad says the row is a failure.
func resultText(r server.Run, now time.Time) (line string, bad bool) {
	mark, what := "✓", "passed"
	switch r.State {
	case store.StateFailed:
		bad = true
		mark, what = "✗", "failed"
		if r.Infra != "" {
			what = "could not run"
		} else if r.Kind == store.KindSuite && r.Failed > 0 && r.Tests > 0 {
			what = fmt.Sprintf("%d/%d failed", r.Failed, r.Tests)
		}
	case store.StateCancelled:
		mark, what = "–", "cancelled"
	case store.StateReused:
		what = "reused"
	default:
		if r.Kind == store.KindSuite && r.Tests > 0 {
			what = fmt.Sprintf("%d/%d passed", r.Passed, r.Tests)
		}
	}
	parts := []string{fmt.Sprintf("%s %s #%s", mark, label(r.Suite), r.ID), what}
	if d := took(r); d > 0 {
		parts = append(parts, fmtDur(d))
	}
	if r.Finished > 0 {
		parts = append(parts, fmtDur(sinceMS(now, r.Finished))+" ago")
	}
	return strings.Join(parts, " · "), bad
}

func sinceMS(now time.Time, ms int64) time.Duration {
	d := now.Sub(time.UnixMilli(ms))
	if d < 0 {
		return 0
	}
	return d.Truncate(time.Second)
}

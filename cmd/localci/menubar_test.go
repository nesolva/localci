package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/server"
)

var menuNow = time.UnixMilli(1_700_000_000_000)

func ago(d time.Duration) int64 { return menuNow.Add(-d).UnixMilli() }

func finished(id, suite, state string, tests, passed, failed int, age time.Duration) server.Run {
	return server.Run{ID: id, Suite: suite, Kind: "suite", State: state, Tests: tests, Passed: passed, Failed: failed,
		Started: ago(age + 5*time.Minute), Finished: ago(age)}
}

func menuText(in menuInput) string {
	in.Now, in.Dashboard, in.Bin = menuNow, "http://127.0.0.1:7357", "/opt/bin/localci"
	var b strings.Builder
	renderMenu(&b, in)
	return b.String()
}

func TestMenuSnapshots(t *testing.T) {
	passed := finished("41", "e2e", "passed", 128, 128, 0, 3*time.Hour)
	failed := finished("42", "e2e", "failed", 128, 126, 2, 20*time.Minute)
	running := server.Run{ID: "43", Suite: "e2e", Kind: "suite", State: "running", Created: ago(3 * time.Minute),
		Started: ago(130 * time.Second), ETAMS: 4 * 60_000}
	waiting := server.Run{ID: "44", Suite: "smoke", Kind: "suite", State: "queued", Created: ago(12 * time.Second)}
	for _, c := range []struct {
		name string
		in   menuInput
	}{
		{"menubar_idle.txt", menuInput{Recent: []server.Run{passed}}},
		{"menubar_running.txt", menuInput{Active: []server.Run{running, waiting}, Recent: []server.Run{passed}}},
		{"menubar_failed.txt", menuInput{Active: []server.Run{running}, Recent: []server.Run{failed, passed}}},
		{"menubar_off.txt", menuInput{Offline: true}},
	} {
		t.Run(c.name, func(t *testing.T) { golden(t, c.name, menuText(c.in)) })
	}
}

// A suite that failed and then passed is green again; another suite's pass does not hide a red one.
func TestMenuFailureClearsOnlyWhenThatSuitePasses(t *testing.T) {
	red := finished("1", "e2e", "failed", 10, 9, 1, time.Hour)
	other := finished("2", "smoke", "passed", 3, 3, 0, time.Minute)
	fixed := finished("3", "e2e", "passed", 10, 10, 0, time.Minute)
	if !failedNow([]server.Run{other, red}) {
		t.Error("a pass of another suite cleared a failure")
	}
	if failedNow([]server.Run{fixed, red}) {
		t.Error("a later pass of the same suite left the title red")
	}
}

func TestMenubarCommand(t *testing.T) {
	t.Run("server down still draws a menu", func(t *testing.T) {
		out, _, code := cli(t, "/nonexistent/api.sock", "menubar")
		if code != 0 || !strings.HasPrefix(out, "localci off") {
			t.Errorf("exit %d, output %q", code, out)
		}
	})
	t.Run("retry posts the run's retry", func(t *testing.T) {
		var got string
		sock := onSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")[:7]
			w.Write([]byte(`{}`))
		}))
		if _, errb, code := cli(t, sock, "menubar", "retry", "42"); code != 0 {
			t.Fatalf("exit %d: %s", code, errb)
		}
		if got != "POST /api/runs/42/retry Bearer " {
			t.Errorf("request %q", got)
		}
	})
}

func TestMenuNamesCannotInjectLinesOrParameters(t *testing.T) {
	evil := "evil\n| bash=/bin/sh param1=-c terminal=false"
	run := server.Run{ID: "43", Suite: evil, Kind: "suite", State: "running", Created: ago(time.Minute), Started: ago(time.Minute)}
	waiting := server.Run{ID: "44", Suite: evil, Kind: "suite", State: "queued", Created: ago(time.Second)}
	failed := finished("42", evil, "failed", 4, 2, 2, time.Minute)
	got := menuText(menuInput{Active: []server.Run{run, waiting}, Recent: []server.Run{failed}})
	for _, l := range strings.Split(got, "\n") {
		_, params, _ := strings.Cut(l, " | ") // the first separator ends the text; only ours may follow
		if strings.Contains(params, "/bin/sh") || strings.Count(l, " | ") > 1 {
			t.Errorf("a name injected a parameter: %q\nfull output:\n%s", l, got)
		}
	}
	// every item stays on one line: title, rule, 2 active, rule, failed with 2 submenu lines, rule, footer.
	if n := strings.Count(got, "\n"); n != 10 {
		t.Errorf("%d lines, want 10:\n%s", n, got)
	}
}

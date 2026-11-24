package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
)

// realServer is the real API over a real store, on a Unix socket; it returns the socket's path.
func realServer(t *testing.T, mutate func(*server.Deps)) (string, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := server.Deps{Store: st, Bus: event.NewBus(), DataDir: t.TempDir()}
	if mutate != nil {
		mutate(&d)
	}
	// The CLI reads the token from the data directory it runs with; share it.
	t.Setenv("XDG_DATA_HOME", filepath.Dir(d.DataDir))
	srvData := filepath.Join(filepath.Dir(d.DataDir), "localci")
	tok, err := server.LoadToken(srvData)
	if err != nil {
		t.Fatal(err)
	}
	d.Token = tok
	return onSocket(t, server.New(d).SocketHandler()), st
}

// realCLI runs a command against a real server without resetting the token location.
func realCLI(t *testing.T, url string, args ...string) (string, int) {
	t.Helper()
	var out, errb strings.Builder
	code := run(append([]string{args[0], "--socket", url}, args[1:]...), &out, &errb)
	if code != 0 && errb.Len() > 0 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), code
}

func putFailure(t *testing.T, st *store.Store, run, title string) {
	t.Helper()
	ctx := t.Context()
	if err := st.PutRun(ctx, store.Run{ID: run, Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed,
		Started: 1000, Finished: 2000}); err != nil && !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	id := run + title
	st.PutTest(ctx, store.Test{Run: run, Shard: 1, ID: id, File: "a.spec.ts", Title: title, Line: 3, Status: "failed",
		Failure: json.RawMessage(`{"step":"click","expected":"x","actual":"y"}`)})
	st.AppendEvent(ctx, store.Event{Run: run, Kind: "test.end", Test: id})
}

// `--since` with the cursor of the last call prints only what is newer, and ends with the next cursor.
func TestSinceWithThePrintedCursorShowsOnlyNewerItems(t *testing.T) {
	url, st := realServer(t, nil)
	putFailure(t, st, "1", "first")

	out, code := realCLI(t, url, "runs", "--since", "0")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "#1 failed") || !strings.HasPrefix(lines[1], "cursor ") {
		t.Fatalf("first call:\n%s", out)
	}
	cursor := strings.TrimPrefix(lines[1], "cursor ")

	putFailure(t, st, "2", "second")
	out, _ = realCLI(t, url, "runs", "--since", cursor)
	if strings.Contains(out, "#1 ") || !strings.Contains(out, "#2 failed") {
		t.Errorf("runs --since %s should list only run 2:\n%s", cursor, out)
	}

	out, _ = realCLI(t, url, "show", "1", "--since", "0")
	cursor = strings.TrimSpace(out[strings.LastIndex(out, "cursor ")+len("cursor "):])
	if !strings.Contains(out, "first") {
		t.Fatalf("show --since 0 should show the failure:\n%s", out)
	}
	putFailure(t, st, "1", "late")
	out, _ = realCLI(t, url, "show", "1", "--since", cursor)
	if strings.Contains(out, "first") || !strings.Contains(out, "late") {
		t.Errorf("show --since %s should show only the new failure:\n%s", cursor, out)
	}
	if _, err := strconv.Atoi(cursor); err != nil {
		t.Errorf("cursor %q is not a number", cursor)
	}
}

// The sample app with ACME_BREAK=1 fails a test; its recorded report (protocol sample) comes out of `show` as a
// short, complete failure.
func TestShowOfTheBrokenSampleHasEverythingInUnderTwentyFiveLines(t *testing.T) {
	url, st := realServer(t, nil)
	ctx := t.Context()
	st.PutRun(ctx, store.Run{ID: "5", Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed, Started: 1000, Finished: 9000})
	f, err := os.Open(filepath.Join("..", "..", "internal", "report", "testdata", "sample.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	found := false
	for sc.Scan() {
		var ev struct {
			T       string          `json:"t"`
			Test    string          `json:"test"`
			Status  string          `json:"status"`
			MS      int64           `json:"ms"`
			Failure json.RawMessage `json:"failure"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.T != "test.end" || len(ev.Failure) == 0 {
			continue
		}
		found = true
		st.PutTest(ctx, store.Test{Run: "5", Shard: 1, ID: ev.Test, File: "tests/cart.spec.ts", Title: "cart › two widgets and a gizmo add up",
			Line: 21, Status: ev.Status, MS: ev.MS, Failure: ev.Failure})
	}
	if !found {
		t.Fatal("the protocol sample has no failed test")
	}
	out, code := realCLI(t, url, "show", "5")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(lines) > 25 {
		t.Errorf("exit %d, %d lines (want at most 25):\n%s", code, len(lines), out)
	}
	for _, part := range []string{"✗ ", "step: ", "expected: ", "actual: ", "aria: ", "console: ", "network: POST ", "files: "} {
		if !strings.Contains(out, part) {
			t.Errorf("missing %q in:\n%s", part, out)
		}
	}
}

type openAdmit struct{}

func (openAdmit) Admit(_ context.Context, _ checks.Cost) (func(), error) { return func() {}, nil }

// `run --checks` plans the checkout's workflows and runs each job through the checks runner: one line per check.
func TestRunChecksQueuesEveryPlannedCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	repo := t.TempDir()
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wf := filepath.Join(repo, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(`name: CI
on: pull_request
jobs:
  unit:
    runs-on: ubuntu-latest
    steps:
      - run: echo unit ok
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo lint ok
`), 0o644)
	sh("init", "-q", "-b", "main")
	sh("add", ".")
	sh("commit", "-q", "-m", "ci")
	sh("remote", "add", "origin", "https://github.com/acme/web.git")
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644) // untracked: left out, like any snapshot

	var cr *checks.Runner
	url, st := realServer(t, func(d *server.Deps) {
		cr = &checks.Runner{Options: checks.Options{DataDir: d.DataDir}, Store: d.Store, Admit: openAdmit{}, Context: t.Context()}
		d.Checks = cr
	})
	t.Cleanup(cr.Wait)
	t.Chdir(repo)

	out, code := realCLI(t, url, "run", "--checks", "--wait")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// The first line of a check is its announcement; which state it shows
	// (queued, preparing, running) depends on how fast the runner picks it up.
	announced := map[string]int{}
	seen := map[string]bool{}
	var passed int
	for _, l := range lines {
		id, _, _ := strings.Cut(l, " ")
		if !seen[id] {
			seen[id] = true
			for _, state := range []string{" queued CI/", " preparing CI/", " running CI/"} {
				if strings.Contains(l, state) {
					announced[id]++
				}
			}
			continue
		}
		if strings.Contains(l, " passed · CI/") {
			passed++
		}
	}
	if code != 0 || len(announced) != 2 || passed != 2 {
		t.Errorf("exit %d, %d checks announced first and %d passed lines, want 2 and 2:\n%s", code, len(announced), passed, out)
	}
	runs, _ := st.Runs(t.Context(), 0)
	for _, r := range runs {
		if r.Kind != store.KindCheck {
			t.Errorf("run %s is a %s, want check", r.ID, r.Kind)
		}
	}
	if len(runs) != 2 {
		t.Errorf("%d runs in the store, want 2", len(runs))
	}
}

// With `serve` down, the CLI says so and exits 3 without sending anything to a TCP port: the write token is for
// the socket, and whatever listens on the configured port may be another user's process.
func TestCLINeverFallsBackToTCP(t *testing.T) {
	var hits atomic.Int32
	fakeTCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Errorf("the CLI sent %s %s with Authorization %q to the TCP port", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer fakeTCP.Close()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	if _, err := server.LoadToken(filepath.Join(data, "localci")); err != nil { // a token exists: there is something to leak
		t.Fatal(err)
	}
	_, port, _ := strings.Cut(strings.TrimPrefix(fakeTCP.URL, "http://"), ":")
	t.Setenv("LOCALCI_SERVER_PORT", port)
	t.Setenv("LOCALCI_URL", fakeTCP.URL)

	for _, args := range [][]string{{"run"}, {"run", "--failed", "3"}, {"wait", "3"}, {"runs"}} {
		var out, errb strings.Builder
		if code := run(args, &out, &errb); code != exitInfra || !strings.Contains(errb.String(), "not running") {
			t.Errorf("localci %v: exit %d, stderr %q; want exit 3 saying localci is not running", args, code, errb.String())
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the TCP port", hits.Load())
	}
}

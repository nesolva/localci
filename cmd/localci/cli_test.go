package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/server"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// cli runs a command line against a server and returns what it printed and its exit code. The write token is
// set up in a throwaway data directory.
func cli(t *testing.T, url string, args ...string) (string, string, int) {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	if _, err := server.LoadToken(filepath.Join(data, "localci")); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run(append([]string{args[0], "--socket", url}, args[1:]...), &out, &errb)
	return out.String(), errb.String(), code
}

// onSocket serves h on a Unix socket in a short temporary directory (t.TempDir paths can exceed the platform's
// socket path limit) and returns the socket's path.
func onSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return filepath.Join(dir, "api.sock")
}

// fake serves canned JSON over a socket, one entry per "METHOD /path" (the query is ignored unless the key has one).
func fake(t *testing.T, routes map[string]any) string {
	t.Helper()
	return onSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{r.Method + " " + r.URL.RequestURI(), r.Method + " " + r.URL.Path} {
			if v, ok := routes[key]; ok {
				if e, isErr := v.(server.Error); isErr {
					w.WriteHeader(http.StatusNotFound)
					json.NewEncoder(w).Encode(e)
					return
				}
				if s, isStr := v.(string); isStr {
					w.Header().Set("X-Log-Path", "/data/logs/42/shard-2.log")
					w.Write([]byte(s))
					return
				}
				json.NewEncoder(w).Encode(v)
				return
			}
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}))
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs\n--- want\n%s--- got\n%s", name, want, got)
	}
}

// The run of the contract's examples: a suite on a working tree that failed 2 of 128 tests.
func failedRun() server.Run {
	return server.Run{ID: "42", Suite: "e2e", Kind: "suite", State: "failed", Created: 1_000, Started: 1_000, Finished: 1_000 + 312_000,
		Heads: []server.Head{{Repo: "acme/web", SHA: "a1b2c3d4e5f6", Dir: "/work/web", Dirty: true}},
		Tests: 128, Passed: 126, Failed: 2, Shards: 4}
}

func queuedRun() server.Run {
	r := failedRun()
	r.State, r.Finished, r.Started, r.Passed, r.Failed = "queued", 0, 0, 0, 0
	r.ETAMS = 6 * 60_000
	return r
}

func checkoutFailure() server.Failure {
	return server.Failure{Test: "t1", Title: "checkout › pays with saved card", File: "tests/cart.spec.ts", Line: 41,
		Shard: 2, MS: 8100, Status: "failed", Step: `click "Pay"`, Expected: "enabled", Actual: "disabled",
		Location: "tests/cart.spec.ts:41", Aria: `- button "Pay" [disabled]`,
		Console:    []string{"TypeError: card is undefined (app.js:210)"},
		Network:    []server.NetworkError{{Method: "POST", URL: "/api/payments", Status: 500}},
		Screenshot: "/data/runs/42/s2/screenshot.png", Trace: "/data/runs/42/s2/trace.zip"}
}

func secondFailure() server.Failure {
	return server.Failure{Test: "t2", Title: "cart › removes a line", File: "tests/cart.spec.ts", Line: 77, Shard: 1, MS: 2300, Status: "failed",
		Step: `click "Remove"`, Message: "locator resolved to 0 elements"}
}

func fixtureRoutes() map[string]any {
	return map[string]any{
		"POST /api/runs":   server.SubmitResult{Runs: []server.Run{queuedRun()}, Cursor: 9},
		"GET /api/runs/42": server.RunDetail{Run: failedRun()},
		"GET /api/queue":   server.Queue{Runs: []server.Run{queuedRun()}},
		"GET /api/workers": server.Workers{Workers: []server.Worker{{Name: "this machine", CPUs: 10, UsedCPUs: 4, Memory: 16 << 30, UsedMemory: 4 << 30, Running: 1}}},
		"GET /api/runs/42/failures": server.FailureList{Run: failedRun(), Total: 2,
			Failures: []server.Failure{checkoutFailure(), secondFailure()}, Cursor: 12},
		"GET /api/runs?limit=11":                 server.RunList{Runs: []server.Run{queuedRun(), failedRun()}, Total: 2, Cursor: 12},
		"GET /api/runs?limit=12":                 server.RunList{Runs: []server.Run{queuedRun(), failedRun()}, Total: 2, Cursor: 12},
		"GET /api/runs/42/log?shard=2&tail=2000": "step 1 ok\nError: card is undefined\n    at app.js:210\nstep 3 skipped\n",
	}
}

func TestGoldenText(t *testing.T) {
	url := fake(t, fixtureRoutes())
	for _, c := range []struct {
		name string
		args []string
		code int
	}{
		{"run.txt", []string{"run", "--suite", "e2e"}, 0},
		{"wait.txt", []string{"wait", "42"}, 1},
		{"show.txt", []string{"show", "42"}, 0},
		{"show_all.txt", []string{"show", "42", "--all"}, 0},
		{"status.txt", []string{"status"}, 0},
		{"queue.txt", []string{"queue"}, 0},
		{"runs.txt", []string{"runs"}, 0},
		{"workers.txt", []string{"workers"}, 0},
		{"logs.txt", []string{"logs", "42", "--shard", "2"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, errb, code := cli(t, url, c.args...)
			if code != c.code {
				t.Errorf("exit %d, want %d (stderr: %s)", code, c.code, errb)
			}
			golden(t, c.name, out)
		})
	}
}

func TestGoldenJSON(t *testing.T) {
	url := fake(t, fixtureRoutes())
	for _, c := range []struct {
		name string
		args []string
	}{
		{"run.json", []string{"run", "--suite", "e2e", "--json"}},
		{"wait.json", []string{"wait", "42", "--json"}},
		{"show.json", []string{"show", "42", "--all", "--json"}},
		{"runs.json", []string{"runs", "--json"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _, _ := cli(t, url, c.args...)
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				var m map[string]any
				if err := json.Unmarshal([]byte(line), &m); err != nil || m["type"] == nil {
					t.Fatalf("not an NDJSON object with a type: %q (%v)", line, err)
				}
			}
			golden(t, c.name, out)
		})
	}
}

// show keeps a failure to 25 lines however much the report carries, and --full lifts the limit.
func TestShowLimitsAFailureToTwentyFiveLines(t *testing.T) {
	f := checkoutFailure()
	f.Aria = strings.Repeat("- row \"x\"\n", 60)
	f.Console = []string{"a", "b", "c", "d", "e"}
	short := failureText(f, false)
	if len(short) > failureLines {
		t.Errorf("%d lines, want at most %d", len(short), failureLines)
	}
	if full := failureText(f, true); len(full) <= failureLines {
		t.Errorf("--full printed %d lines, expected the whole report", len(full))
	}
}

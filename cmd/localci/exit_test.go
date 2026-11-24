package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/server"
)

func runIn(state, kind string, failed int) server.Run {
	return server.Run{ID: "7", Suite: "e2e", Kind: kind, State: state, Started: 1000, Finished: 6000, Tests: 10, Passed: 10 - failed, Failed: failed}
}

func withInfra(r server.Run) server.Run {
	r.Infra = "stack app exited before the run ended (code 137)"
	return r
}

// An infrastructure failure says so on the summary line, and the line is the one an agent reads next.
func TestSummaryLineNamesTheInfrastructureFailure(t *testing.T) {
	r := withInfra(runIn("failed", "suite", 0))
	r.Infra = "tenant flow make-tenant failed: \x1b[31mPOST /tenants answered 500\x1b[0m"
	got := finishedLine(r, false)
	if !strings.Contains(got, "infrastructure: tenant flow make-tenant failed: \ufffd[31mPOST") || strings.Contains(got, "\x1b") {
		t.Fatalf("line = %q", got)
	}
	if strings.Contains(got, "0/10") || !strings.Contains(got, "localci logs 7") {
		t.Fatalf("line = %q: an infrastructure failure is not a count of failed tests", got)
	}
}

// An agent branches on the exit code: 0 passed, 1 tests failed, 2 usage, 3 infrastructure, 4 timeout.
func TestExitCodes(t *testing.T) {
	pollEvery = time.Millisecond
	t.Cleanup(func() { pollEvery = time.Second })

	route := func(r server.Run) map[string]any {
		return map[string]any{"GET /api/runs/7": server.RunDetail{Run: r}}
	}
	down := httptest.NewServer(nil)
	downURL := down.URL
	down.Close()

	for _, c := range []struct {
		name   string
		routes map[string]any
		url    string
		args   []string
		want   int
	}{
		{"passed", route(runIn("passed", "suite", 0)), "", []string{"wait", "7"}, 0},
		{"reused check", route(runIn("reused", "check", 0)), "", []string{"wait", "7"}, 0},
		{"tests failed", route(runIn("failed", "suite", 2)), "", []string{"wait", "7"}, 1},
		{"a failing check", route(runIn("failed", "check", 0)), "", []string{"wait", "7"}, 1},
		{"broke before its tests", route(runIn("failed", "suite", 0)), "", []string{"wait", "7"}, 3},
		{"infrastructure with tests interrupted", route(withInfra(runIn("failed", "suite", 2))), "", []string{"wait", "7"}, 3},
		{"cancelled", route(runIn("cancelled", "suite", 0)), "", []string{"wait", "7"}, 3},
		{"no id given", nil, "", []string{"wait"}, 2},
		{"unknown run", map[string]any{"GET /api/runs/7": server.Error{Error: "no run 7"}}, "", []string{"wait", "7"}, 2},
		{"unknown command", nil, "", []string{"frobnicate"}, 2},
		{"bad flag", nil, "", []string{"show", "7", "--nope"}, 2},
		{"server not running", nil, downURL, []string{"wait", "7"}, 3},
		{"gave up waiting", route(runIn("running", "suite", 0)), "", []string{"wait", "7", "--timeout", "30ms"}, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := c.url
			if url == "" {
				url = fake(t, c.routes)
			}
			_, errb, code := cli(t, url, c.args...)
			if code != c.want {
				t.Errorf("exit %d, want %d (stderr: %s)", code, c.want, errb)
			}
		})
	}
}

func TestServerDownSaysHowToStartIt(t *testing.T) {
	down := httptest.NewServer(nil)
	url := down.URL
	down.Close()
	_, errb, code := cli(t, url, "status")
	if code != exitInfra || !strings.Contains(errb, "localci serve") {
		t.Errorf("exit %d, stderr %q: want 3 and the command that starts the server", code, errb)
	}
}

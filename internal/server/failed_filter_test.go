package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// The API refuses `failed` with 400, before it creates a run, unless the run named is one of this suite with failed
// tests. Anything else would select nothing, or another suite's test names.
func TestRunFailedRefused(t *testing.T) {
	suites, err := suite.ParseYAML([]byte("suites:\n  api:\n    repos: [acme/web]\n    test: {run: \"true\", list: \"true\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := newRigWith(t, func(d *Deps) {
		missing := t.TempDir() + "/no-repo"
		r, err := suite.New(suite.Deps{Store: d.Store, Log: event.NewLog(d.Store, d.Bus), Sched: sched.New(1, 0),
			DataDir: t.TempDir(), Suites: suites, RepoURL: func(string) string { return missing }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		d.Suites = r
	})
	ctx := t.Context()
	for _, r := range []store.Run{
		{ID: "41", Suite: "web", Kind: store.KindSuite, State: store.StateFailed},
		{ID: "42", Suite: "api", Kind: store.KindSuite, State: store.StatePassed},
		{ID: "43", Suite: "ci/test", Kind: store.KindCheck, State: store.StateFailed},
		{ID: "44", Suite: "api", Kind: store.KindSuite, State: store.StateFailed},
	} {
		if err := g.st.PutRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{"41", "44"} {
		if err := g.st.PutTest(ctx, store.Test{Run: run, Shard: 1, ID: "x", Title: "x", Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(failed string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", sockURL+"/api/runs", strings.NewReader(`{"suite":"api","failed":"`+failed+`"}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := g.sock.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for id, want := range map[string]string{
		"41": "run 41 is of suite web, not api",
		"42": "run 42 has no failed tests",
		"43": "run 43 is a check, not a run of suite api",
	} {
		if code, body := post(id); code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("failed=%s: %d %s, want 400 and %q", id, code, body, want)
		}
	}
	if runs, err := g.st.Runs(ctx, 0); err != nil || len(runs) != 4 {
		t.Errorf("a refused request created a run: %d runs (%v)", len(runs), err)
	}
	if code, body := post("44"); code != http.StatusAccepted {
		t.Errorf("failed=44, a run of this suite with a failure: %d %s, want 202", code, body)
	}
}

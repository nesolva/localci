package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// suiteCheckout starts a real server and runner for the suite `e2e` (yaml is what goes under it, after repos), makes a
// checkout of acme/web and moves into it, so `localci run` works as it does from a developer's repository.
func suiteCheckout(t *testing.T, yaml string, affector suite.Affector) (string, *store.Store) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	pollEvery = 20 * time.Millisecond
	t.Cleanup(func() { pollEvery = time.Second })
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.spec.ts"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "first"},
		{"remote", "add", "origin", "https://github.com/acme/web.git"}} {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	suites, err := suite.ParseYAML([]byte("suites:\n  e2e:\n    repos: [acme/web]\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	url, st := realServer(t, func(d *server.Deps) {
		r, err := suite.New(suite.Deps{Store: d.Store, Log: event.NewLog(d.Store, d.Bus), Sched: sched.New(2, 0), DataDir: d.DataDir,
			Suites: suites, RepoURL: func(string) string { return repo }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		if affector != nil {
			r.SetAffector(affector)
		}
		d.Suites = r
	})
	t.Chdir(repo)
	return url, st
}

const oneTestSuite = `    test:
      run: "true"
      list: printf '{"test":"a","file":"a.spec.ts","title":"a","line":1}\n'
    shards: 1
`

// cliBoth is realCLI with what the command wrote to stderr.
func cliBoth(t *testing.T, url string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb strings.Builder
	code = run(append([]string{args[0], "--socket", url}, args[1:]...), &out, &errb)
	return out.String(), errb.String(), code
}

// A file filter that selects nothing is a failed run and exit 2, from `run --wait` and from a later `wait`, also when
// another run waited with it passed.
func TestRunFilterMatchesNothing(t *testing.T) {
	url, st := suiteCheckout(t, oneTestSuite, nil)

	out, errb, code := cliBoth(t, url, "run", "gone.spec.ts", "--wait")
	if code != 2 || !strings.Contains(out, "no tests matched gone.spec.ts") {
		t.Fatalf("exit %d, want 2 and the reason\nstdout: %s\nstderr: %s", code, out, errb)
	}
	runs, err := st.Runs(context.Background(), 0)
	if err != nil || len(runs) != 1 || runs[0].State != store.StateFailed {
		t.Fatalf("stored runs: %+v (%v), want one failed run", runs, err)
	}
	if err := st.PutRun(t.Context(), store.Run{ID: "90", Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed}); err != nil {
		t.Fatal(err)
	}
	// With several runs the exit code is the worst one's; exit 2 must not count as 0.
	if out, _, code := cliBoth(t, url, "wait", "90", runs[0].ID); code != 2 {
		t.Errorf("wait: exit %d, want 2\n%s", code, out)
	}
}

// --failed is refused, with no run created, when the run it names cannot give failed tests of this suite.
func TestRunFailedRefused(t *testing.T) {
	url, st := suiteCheckout(t, oneTestSuite, nil)
	ctx := t.Context()
	for _, r := range []store.Run{
		{ID: "41", Suite: "web", Kind: store.KindSuite, State: store.StateFailed},
		{ID: "42", Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed},
	} {
		if err := st.PutRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutTest(ctx, store.Test{Run: "41", Shard: 1, ID: "x", File: "x.spec.ts", Title: "x", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"41": "run 41 is of suite web, not e2e", "42": "run 42 has no failed tests"} {
		out, errb, code := cliBoth(t, url, "run", "--failed", id, "--wait")
		if code != 2 || !strings.Contains(errb, want) {
			t.Errorf("--failed %s: exit %d, stderr %q, want 2 and %q\n%s", id, code, errb, want, out)
		}
	}
	if runs, err := st.Runs(ctx, 0); err != nil || len(runs) != 2 {
		t.Errorf("a refused --failed created a run: %d runs (%v)", len(runs), err)
	}
}

type nothingAffected struct{}

func (nothingAffected) Plan(context.Context, suite.PlanInput) (suite.Plan, error) {
	return suite.Plan{Detail: []byte(`{"all":false,"reason":"nothing the tests reach changed"}`)}, nil
}
func (nothingAffected) Done(context.Context, suite.DoneInput) {}

// A diff that no test can see is a pass, and the line says why nothing ran.
func TestAffectedNothingIsPass(t *testing.T) {
	url, _ := suiteCheckout(t, oneTestSuite, nothingAffected{})
	out, errb, code := cliBoth(t, url, "run", "--affected", "--wait")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if code != 0 || !strings.Contains(last, "passed") || !strings.Contains(last, "no tests affected") {
		t.Fatalf("exit %d, want 0 and a passed line with the reason\nstdout: %s\nstderr: %s", code, out, errb)
	}
}

// A run its suite timeout stopped exits 4, although the test it interrupted is counted as failed, and the line says
// that it timed out.
func TestSuiteTimeoutExit4(t *testing.T) {
	url, st := suiteCheckout(t, `    test:
      run: |
        printf '{"v":1,"t":"begin","ts":1,"shard":1,"total":1}\n{"v":1,"t":"test.begin","ts":1,"test":"a","file":"a.spec.ts","title":"a","line":1}\n' >> "$LOCALCI_REPORT"
        exec sleep 300
      list: printf '{"test":"a","file":"a.spec.ts","title":"a","line":1}\n'
    shards: 1
    timeout: 3s
`, nil)
	out, errb, code := cliBoth(t, url, "run", "--wait")
	if code != 4 || !strings.Contains(out, "timed out after 3s") {
		t.Fatalf("exit %d, want 4 and `timed out after 3s`\nstdout: %s\nstderr: %s", code, out, errb)
	}
	runs, err := st.Runs(context.Background(), 0)
	if err != nil || len(runs) != 1 || runs[0].State != store.StateFailed {
		t.Fatalf("stored runs: %+v (%v), want one failed run", runs, err)
	}
	if c, err := st.TestCounts(t.Context(), runs[0].ID); err != nil || c.Failed == 0 {
		t.Errorf("counts %+v (%v): the case is a timeout with an interrupted test", c, err)
	}
}

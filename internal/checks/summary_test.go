package checks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
)

// gatedRun is a step that stays running until the test opens the gate.
const gatedRun = `while [ ! -e "$GATE" ]; do sleep 0.05; done`

// newGate sets $GATE and returns what opens it. The cleanup opens it too and
// waits, so a failed test never leaves a step waiting for ever.
func newGate(t *testing.T, fx *fixture) func() {
	t.Helper()
	gate := filepath.Join(t.TempDir(), "gate")
	t.Setenv("GATE", gate)
	open := func() {
		if err := os.WriteFile(gate, nil, 0o644); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { open(); fx.runner.Wait() })
	return open
}

// jobs is a workflow with one job per name and run line.
func jobs(nameRun ...string) string {
	wf := "name: CI\non: [pull_request]\njobs:\n"
	for i := 0; i < len(nameRun); i += 2 {
		wf += "  " + nameRun[i] + ":\n    runs-on: ubuntu-latest\n    steps:\n      - run: '" + nameRun[i+1] + "'\n"
	}
	return wf
}

// summaries are the `localci/checks` statuses posted so far, as "state: description".
func summaries(fx *fixture) []string {
	var out []string
	for _, p := range fx.forge.of("checks") {
		out = append(out, string(p.State)+": "+p.Description)
	}
	return out
}

// awaitSummaries waits until n summaries were posted: a check's summary goes
// out after its Done is closed.
func awaitSummaries(t *testing.T, fx *fixture, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got := summaries(fx); len(got) >= n {
			return got
		} else if time.Now().After(deadline) {
			t.Fatalf("localci/checks = %q, want %d statuses", got, n)
		}
	}
}

// wantPendingThen fails unless got is one or more pendings and then only final,
// which checks that end together may each post.
func wantPendingThen(t *testing.T, got []string, final string) {
	t.Helper()
	i := 0
	for i < len(got) && strings.HasPrefix(got[i], "pending: ") {
		i++
	}
	if i == 0 || i == len(got) {
		t.Fatalf("localci/checks = %q, want pending and then %q", got, final)
	}
	for _, s := range got[i:] {
		if s != final {
			t.Fatalf("localci/checks = %q, want only %q once nothing is pending", got, final)
		}
	}
}

// awaitFile waits for a step to create path.
func awaitFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("no step created %s", path)
		}
	}
}

func TestChecksSummaryWaitsForEveryJob(t *testing.T) {
	fx := newFixture(t, jobs("green", gatedRun, "red", "exit 1"))
	open := newGate(t, fx)
	head := fx.pull("1", map[string]string{"a": "1"})
	sub := fx.start(fx.request(1, head))

	if got := summaries(fx); len(got) == 0 || got[0] != "pending: 2 checks running" {
		t.Fatalf("localci/checks right after Submit = %q, want pending first", got)
	}
	for _, r := range sub.Runs {
		if r.Name == "red" {
			fx.await(r)
		}
	}
	if got := awaitSummaries(t, fx, 2); got[1] != "pending: 1 check running" {
		t.Fatalf("localci/checks with one check still running = %q", got)
	}
	open()
	fx.runner.Wait()
	wantPendingThen(t, summaries(fx), "failure: 1 passed, 1 failed")
	for _, p := range fx.forge.of("checks") {
		if p.Repo != "acme/web" || p.SHA != head {
			t.Fatalf("summary went to %s@%s, want the PR head %s", p.Repo, p.SHA, head)
		}
	}
}

func TestChecksSummaryWithoutMergeRef(t *testing.T) {
	fx := newFixture(t, ciWorkflow)
	fx.runner.Options.MergeRef = false
	fx.submit(fx.request(1, fx.pull("1", map[string]string{"a": "1"})))
	fx.runner.Wait()
	wantPendingThen(t, summaries(fx), "success: 1 passed")
}

func TestChecksSummarySuperseded(t *testing.T) {
	t.Run("by a newer head", func(t *testing.T) {
		fx := newFixture(t, jobs("slow", gatedRun))
		open := newGate(t, fx)
		old := fx.pull("1", map[string]string{"a": "1"})
		first := fx.start(fx.request(1, old)).Runs[0]
		newer := fx.rebase("1")
		fx.start(fx.request(1, newer))
		fx.await(first)
		open()
		fx.runner.Wait()

		wantPendingThen(t, summaries(fx), "success: 1 passed")
		got := fx.forge.of("checks")
		if last := got[len(got)-1]; last.SHA != newer {
			t.Fatalf("the final summary went to %s, want the new head %s", last.SHA, newer)
		}
		for _, p := range got {
			if p.SHA == old && p.State != forge.StatePending {
				t.Fatalf("the head that moved got a final summary: %+v", p)
			}
		}
	})
	t.Run("by a second submit of the same head", func(t *testing.T) {
		fx := newFixture(t, jobs("slow", gatedRun))
		open := newGate(t, fx)
		head := fx.pull("1", map[string]string{"a": "1"})
		first := fx.start(fx.request(1, head)).Runs[0]
		fx.start(fx.request(1, head))
		fx.await(first)
		open()
		fx.runner.Wait()
		wantPendingThen(t, summaries(fx), "success: 1 passed")
	})
}

// A retry is a submit of one check: it must not turn the summary green while a
// sibling is red.
func TestChecksSummaryAfterRetry(t *testing.T) {
	fx := newFixture(t, jobs("green", `if [ -e "$HOLD" ]; then `+gatedRun+`; fi`, "red", "exit 1"))
	open := newGate(t, fx)
	hold := filepath.Join(t.TempDir(), "hold")
	t.Setenv("HOLD", hold)
	head := fx.pull("1", map[string]string{"a": "1"})
	fx.submit(fx.request(1, head))
	fx.runner.Wait()
	wantPendingThen(t, summaries(fx), "failure: 1 passed, 1 failed")

	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(summaries(fx))
	retry := fx.request(1, head)
	retry.Only, retry.Force = "localci/CI/green", true
	if sub := fx.start(retry); len(sub.Runs) != 1 {
		t.Fatalf("the retry started %d checks, want 1", len(sub.Runs))
	}
	if got := summaries(fx)[before:]; len(got) == 0 || got[0] != "pending: 1 check running" {
		t.Fatalf("localci/checks while the retry runs = %q", got)
	}
	open()
	fx.runner.Wait()
	wantPendingThen(t, summaries(fx)[before:], "failure: 1 passed, 1 failed")
}

// `localci run --checks` on a clean checkout records the pull request's head
// commit too. It posts nothing, so its green must not hide the pull request's
// red.
func TestChecksSummaryIgnoresLocalRuns(t *testing.T) {
	fx := newFixture(t, jobs("env", `test -z "$FAIL"`, "other", "true"))
	head := fx.pull("1", map[string]string{"a": "1"})
	t.Setenv("FAIL", "1")
	fx.submit(fx.request(1, head))
	fx.runner.Wait()

	t.Setenv("FAIL", "")
	fx.submit(Request{Repo: "acme/web", Snapshot: head, Trigger: "manual", Force: true})
	retry := fx.request(1, head)
	retry.Only, retry.Force = "localci/CI/other", true
	fx.submit(retry)
	fx.runner.Wait()
	if got := summaries(fx); len(got) == 0 || got[len(got)-1] != "failure: 1 passed, 1 failed" {
		t.Fatalf("localci/checks = %q, want it to stay red", got)
	}
}

// Only a pass or a reused pass is green; a check that broke, was cancelled or
// never ran is not.
func TestChecksSummaryIsNotGreenUnlessEveryCheckPassed(t *testing.T) {
	// An action-based job goes to Act, which here cannot run it.
	const broken = "  act:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/action@v1\n"
	brokenAct := func(context.Context, ActJob) (exec.Result, error) { return exec.Result{}, errors.New("act broke") }

	for name, c := range map[string]struct{ workflow, want string }{
		"infrastructure":                     {jobs("ok", "true") + broken, "error: 1 passed, 1 errored"},
		"a failure outweighs a broken check": {jobs("ok", "true", "red", "exit 1") + broken, "failure: 1 passed, 1 failed, 1 errored"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, c.workflow)
			fx.runner.Act = brokenAct
			fx.submit(fx.request(1, fx.pull("1", map[string]string{"a": "1"})))
			fx.runner.Wait()
			wantPendingThen(t, summaries(fx), c.want)
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		fx := newFixture(t, jobs("slow", `touch "$GATE.up"; `+gatedRun))
		newGate(t, fx)
		run := fx.start(fx.request(1, fx.pull("1", map[string]string{"a": "1"}))).Runs[0]
		awaitFile(t, os.Getenv("GATE")+".up")
		fx.runner.Cancel(run.RunID)
		fx.runner.Wait()
		wantPendingThen(t, summaries(fx), "error: 1 cancelled")
	})
	t.Run("never run", func(t *testing.T) {
		fx := newFixture(t, jobs("one", "true", "two", "true"))
		only := fx.request(1, fx.pull("1", map[string]string{"a": "1"}))
		only.Only = "localci/CI/one"
		fx.submit(only)
		fx.runner.Wait()
		wantPendingThen(t, summaries(fx), "error: 1 passed, 1 not run")
	})
}

func TestChecksSummaryNothingApplies(t *testing.T) {
	const needsAct = "  act:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: acme/action@v1\n"
	for name, workflowYAML := range map[string]string{
		"no workflow runs on pull requests": "name: CI\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: exit 1\n",
		"every check is skipped":            jobs() + needsAct,
	} {
		t.Run(name, func(t *testing.T) {
			fx := newFixture(t, workflowYAML)
			sub := fx.submit(fx.request(1, fx.pull("1", map[string]string{"a": "1"})))
			if len(sub.Runs) != 0 {
				t.Fatalf("runs = %+v, want none", sub.Runs)
			}
			if got := summaries(fx); len(got) != 1 || got[0] != "success: no checks apply" {
				t.Fatalf("localci/checks = %q", got)
			}
		})
	}
	t.Run("a skipped check does not count", func(t *testing.T) {
		fx := newFixture(t, jobs("ok", "true")+needsAct)
		if sub := fx.submit(fx.request(1, fx.pull("1", map[string]string{"a": "1"}))); len(sub.Skipped) != 1 {
			t.Fatalf("skips = %+v, want the check that needs act", sub.Skipped)
		}
		fx.runner.Wait()
		wantPendingThen(t, summaries(fx), "success: 1 passed")
	})
}

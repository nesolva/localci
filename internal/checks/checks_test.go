package checks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

func TestRebasedPullRequestWithTheSameTreeIsReused(t *testing.T) {
	fx := newFixture(t, ciWorkflow)
	head := fx.pull("1", map[string]string{"feature.txt": "x\n"})

	first := fx.submit(fx.request(1, head)).Runs[0]
	if got := fx.run(first.RunID).State; got != store.StatePassed {
		t.Fatalf("first run: %s\n%s", got, fx.runLog(first.RunID))
	}

	newHead := fx.rebase("1")
	second := fx.submit(fx.request(1, newHead)).Runs[0]
	r := fx.run(second.RunID)
	if r.State != store.StateReused || !strings.Contains(r.Reason, "#"+first.RunID) {
		t.Fatalf("rebased run = %s %q, want reused #%s", r.State, r.Reason, first.RunID)
	}
	if n := executions(t); n != 1 {
		t.Fatalf("steps ran %d times, want 1", n)
	}
	last := fx.forge.of("CI/build")
	if got := last[len(last)-1]; got.State != "success" || got.Description != "reused #"+first.RunID || got.SHA != newHead {
		t.Fatalf("status = %s %q on %s", got.State, got.Description, got.SHA)
	}

	// A different tree is a different result.
	head2 := fx.pull("2", map[string]string{"feature.txt": "y\n"})
	third := fx.submit(fx.request(2, head2)).Runs[0]
	if st := fx.run(third.RunID).State; st != store.StatePassed {
		t.Fatalf("third run: %s", st)
	}
	if n := executions(t); n != 2 {
		t.Fatalf("steps ran %d times, want 2", n)
	}

	// Force runs it anyway.
	req := fx.request(1, newHead)
	req.Force = true
	forced := fx.submit(req).Runs[0]
	if st := fx.run(forced.RunID).State; st != store.StatePassed || executions(t) != 3 {
		t.Fatalf("forced run: %s, %d executions", st, executions(t))
	}
}

func TestAFailedRunIsNeverReused(t *testing.T) {
	fx := newFixture(t, strings.Replace(ciWorkflow, `echo ran >> "$RUNLOG"`, `echo ran >> "$RUNLOG"; exit 1`, 1))
	head := fx.pull("1", map[string]string{"a": "1"})
	fx.submit(fx.request(1, head))
	second := fx.submit(fx.request(1, fx.rebase("1"))).Runs[0]
	if st := fx.run(second.RunID).State; st != store.StateFailed || executions(t) != 2 {
		t.Fatalf("second run: %s, %d executions", st, executions(t))
	}
}

const twoJobs = `name: CI
on: [pull_request]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo linted
  test:
    name: unit tests
    runs-on: ubuntu-latest
    steps:
      - name: compile
        run: echo compiling
      - name: run the suite
        run: echo "1 failing" >&2; exit 3
      - name: not reached
        run: echo nope
      - name: cleanup
        if: always()
        run: echo tidy
`

func TestEachCheckPostsItsOwnStatusOnTheHead(t *testing.T) {
	fx := newFixture(t, twoJobs)
	head := fx.pull("7", map[string]string{"a": "1"})
	sub := fx.submit(fx.request(7, head))
	if len(sub.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(sub.Runs))
	}
	byName := map[string]Started{}
	for _, r := range sub.Runs {
		byName[r.Name] = r
	}
	if got := byName["unit tests"].Context; got != "localci/CI/unit tests" {
		t.Fatalf("context = %q", got)
	}

	lint := fx.forge.of("CI/lint")
	if len(lint) != 3 || lint[0].State != "pending" || lint[len(lint)-1].State != "success" {
		t.Fatalf("lint statuses = %+v", lint)
	}
	if !strings.HasPrefix(lint[0].Description, "queued") || !strings.HasPrefix(lint[1].Description, "running") {
		t.Fatalf("lint descriptions = %q, %q", lint[0].Description, lint[1].Description)
	}
	unit := fx.forge.of("CI/unit tests")
	end := unit[len(unit)-1]
	if end.State != "failure" || end.Description != "1 failed: run the suite" {
		t.Fatalf("unit tests final status = %s %q", end.State, end.Description)
	}
	for _, p := range append(lint, unit...) {
		if p.Repo != "acme/web" || p.SHA != head {
			t.Fatalf("status went to %s@%s, want the PR head %s", p.Repo, p.SHA, head)
		}
		if p.TargetURL == "" {
			t.Fatalf("status without a link: %+v", p)
		}
	}

	log := fx.runLog(byName["unit tests"].RunID)
	for _, want := range []string{"==> compile", "1 failing", "==> cleanup"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "==> not reached") {
		t.Errorf("a step after the failure ran:\n%s", log)
	}
	if st := fx.run(byName["unit tests"].RunID); st.State != store.StateFailed {
		t.Fatalf("state = %s", st.State)
	}
}

func TestChecksSharingADockerTagNeverOverlap(t *testing.T) {
	wf := `name: Scan
on: [pull_request]
jobs:
  scan-a:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t app:scan .
  scan-b:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t app:scan .
  other:
    runs-on: ubuntu-latest
    steps:
      - run: docker build -t other:scan .
`
	fx := newFixture(t, wf)
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "docker.trace")
	// A docker that takes a while and says when each call started and ended.
	script := "#!/bin/sh\necho \"start $3\" >> " + trace + "\nsleep 0.4\necho \"end $3\" >> " + trace + "\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	head := fx.pull("3", map[string]string{"a": "1"})
	for _, r := range fx.submit(fx.request(3, head)).Runs {
		if st := fx.run(r.RunID).State; st != store.StatePassed {
			t.Fatalf("%s: %s\n%s", r.Name, st, fx.runLog(r.RunID))
		}
	}

	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Fields(line)
		if f[0] == "start" {
			open[f[1]]++
			if open[f[1]] > 1 {
				t.Fatalf("two builds of %s overlapped:\n%s", f[1], b)
			}
		} else {
			open[f[1]]--
		}
	}
	if strings.Count(string(b), "start app:scan") != 2 {
		t.Fatalf("both scans should have run:\n%s", b)
	}
	if fx.admit.max < 2 {
		t.Fatalf("checks were not admitted together (max %d), so the tag lock was not what kept the scans apart", fx.admit.max)
	}
}

func TestNewHeadSupersedesTheRunningCheck(t *testing.T) {
	wf := `name: CI
on: [pull_request]
jobs:
  slow:
    runs-on: ubuntu-latest
    steps:
      - run: echo up > "$UP"; sleep 60
`
	fx := newFixture(t, wf)
	up := filepath.Join(t.TempDir(), "up")
	sh(t, "", "mkfifo", up)
	t.Setenv("UP", up)

	head := fx.pull("4", map[string]string{"a": "1"})
	old := fx.start(fx.request(4, head)).Runs[0]
	if _, err := os.ReadFile(up); err != nil { // returns once the step is running
		t.Fatal(err)
	}
	newer := fx.start(fx.request(4, fx.rebase("4"))).Runs[0]

	fx.await(old)
	if r := fx.run(old.RunID); r.State != store.StateCancelled || r.Reason != "superseded" {
		t.Fatalf("old run = %s %q", r.State, r.Reason)
	}
	if _, err := os.ReadFile(up); err != nil {
		t.Fatal(err)
	}
	fx.runner.Cancel(newer.RunID)
	fx.await(newer)
	if r := fx.run(newer.RunID); r.State != store.StateCancelled || r.Reason != "cancelled" {
		t.Fatalf("cancelled run = %s %q", r.State, r.Reason)
	}
}

func TestWorkingTreeSnapshotRunsWithoutStatuses(t *testing.T) {
	fx := newFixture(t, ciWorkflow)
	checkout := t.TempDir()
	sh(t, checkout, "git", "clone", "-q", fx.src, ".")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("uncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := fx.runner.Snapshot(t.Context(), "acme/web", checkout)
	if err != nil || !snap.Dirty {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	sub := fx.submit(Request{Repo: "acme/web", Snapshot: snap.SHA, Trigger: "manual"})
	if st := fx.run(sub.Runs[0].RunID).State; st != store.StatePassed {
		t.Fatalf("state = %s\n%s", st, fx.runLog(sub.Runs[0].RunID))
	}
	if n := fx.forge.count(); n != 0 {
		t.Fatalf("a working tree has no pull request, yet %d statuses were posted", n)
	}
}

// A moving version ("20") must not reuse a result proven with an older release.
func TestReuseFollowsTheResolvedToolchain(t *testing.T) {
	wf := `name: CI
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/setup-node@v4
        with:
          node-version: 20
      - run: echo ran >> "$RUNLOG"
`
	fx := newFixture(t, wf)
	fx.runner.Options.Toolchains = "mise"
	bin := t.TempDir()
	latest := filepath.Join(t.TempDir(), "latest")
	trace := filepath.Join(t.TempDir(), "mise.trace")
	script := `#!/bin/sh
case "$1" in
latest) cat "` + latest + `" ;;
exec) shift
  while [ "$1" != "--" ]; do echo "$1" >> "` + trace + `"; shift; done
  shift; exec "$@" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	setLatest := func(v string) {
		if err := os.WriteFile(latest, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	head := fx.pull("5", map[string]string{"a": "1"})
	setLatest("20.11.1")
	first := fx.submit(fx.request(5, head)).Runs[0]
	if st := fx.run(first.RunID).State; st != store.StatePassed {
		t.Fatalf("first: %s\n%s", st, fx.runLog(first.RunID))
	}
	if b, _ := os.ReadFile(trace); !strings.Contains(string(b), "node@20.11.1") {
		t.Fatalf("the step did not run with the declared tool; mise saw %q", b)
	}

	same := fx.submit(fx.request(5, fx.rebase("5"))).Runs[0]
	if st := fx.run(same.RunID).State; st != store.StateReused {
		t.Fatalf("same toolchain: %s", st)
	}

	setLatest("20.12.0")
	newer := fx.submit(fx.request(5, fx.rebase("5"))).Runs[0]
	if st := fx.run(newer.RunID).State; st != store.StatePassed || executions(t) != 2 {
		t.Fatalf("newer toolchain: %s, %d executions", st, executions(t))
	}
}

// A workflow the user excluded (a release or a deploy) never runs here.
func TestSkippedWorkflowFilesDoNotRun(t *testing.T) {
	fx := newFixture(t, ciWorkflow)
	fx.write(".github/workflows/release.yml", strings.Replace(ciWorkflow, "name: CI", "name: Release", 1))
	sh(t, fx.src, "git", "add", "-A")
	sh(t, fx.src, "git", "commit", "-q", "-m", "release workflow")
	fx.base = sh(t, fx.src, "git", "rev-parse", "HEAD")
	fx.runner.Options.SkipWorkflows = map[string][]string{"acme/web": {"release.yml"}, "acme/other": {"ci.yml"}}

	sub := fx.submit(fx.request(1, fx.pull("1", map[string]string{"a": "1"})))
	if len(sub.Runs) != 1 || sub.Runs[0].Workflow != "CI" {
		t.Fatalf("runs = %+v, want only CI", sub.Runs)
	}
	var skipped bool
	for _, s := range sub.Skipped {
		skipped = skipped || strings.Contains(s.Workflow, "release.yml") && strings.Contains(s.Reason, "workflows.skip")
	}
	if !skipped {
		t.Fatalf("skips = %+v, want release.yml shown as skipped by the configuration", sub.Skipped)
	}
	if executions(t) != 1 || len(fx.forge.of("Release/build")) != 0 {
		t.Fatalf("%d executions, release statuses %+v", executions(t), fx.forge.of("Release/build"))
	}
}

// The pull request moved on after the trigger: its new head must not be tested
// and reported as the requested one.
func TestAHeadThatMovedIsNotTestedInItsPlace(t *testing.T) {
	for _, mergeRef := range []bool{true, false} {
		fx := newFixture(t, ciWorkflow)
		fx.runner.Options.MergeRef = mergeRef
		old := fx.pull("1", map[string]string{"a": "1"})
		fx.rebase("1") // pushed again before the check started
		_, err := fx.runner.Submit(t.Context(), fx.request(1, old))
		var moved *PullRefError
		if !errors.As(err, &moved) || !moved.Infrastructure() {
			t.Fatalf("merge_ref=%v: Submit = %v, want a PullRefError", mergeRef, err)
		}
		if n := fx.forge.count(); n != 0 || executions(t) != 0 {
			t.Fatalf("merge_ref=%v: %d statuses and %d executions for a head that was not tested", mergeRef, n, executions(t))
		}
	}
}

// GitHub builds refs/pull/N/merge lazily: right after a push it can still be
// the merge of the previous head. Submit says so at once (the watcher's next
// poll is the retry, Submit must not stall it) and posts nothing.
func TestAStaleMergeRefIsNotTestedForTheNewHead(t *testing.T) {
	fx := newFixture(t, ciWorkflow)
	fx.pull("1", map[string]string{"a": "1"})
	staleMerge := sh(t, fx.src, "git", "rev-parse", "refs/pull/1/merge")
	head := fx.rebase("1")
	freshMerge := sh(t, fx.src, "git", "rev-parse", "refs/pull/1/merge")
	sh(t, fx.src, "git", "update-ref", "refs/pull/1/merge", staleMerge)

	start := time.Now()
	_, err := fx.runner.Submit(t.Context(), fx.request(1, head))
	var stale *PullRefError
	if !errors.As(err, &stale) || fx.forge.count() != 0 {
		t.Fatalf("Submit = %v with %d statuses, want a PullRefError and none", err, fx.forge.count())
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Submit waited %s for GitHub inside the watcher's callback", took)
	}

	// The last attempt the watcher makes gives up visibly on the head.
	last := fx.request(1, head)
	last.LastAttempt = true
	if _, err := fx.runner.Submit(t.Context(), last); !errors.As(err, &stale) {
		t.Fatalf("Submit = %v, want a PullRefError", err)
	}
	if ps := fx.forge.statuses; len(ps) != 1 || ps[0].SHA != head || ps[0].State != forge.StateError ||
		!strings.Contains(ps[0].Description, "GitHub merge ref not ready") {
		t.Fatalf("statuses after giving up = %+v, want one error on the head", ps)
	}

	// GitHub caught up by the next poll: the fresh merge is tested.
	sh(t, fx.src, "git", "update-ref", "refs/pull/1/merge", freshMerge)
	sub := fx.submit(fx.request(1, head))
	if r := fx.run(sub.Runs[0].RunID); r.State != store.StatePassed {
		t.Fatalf("after the merge ref caught up: %s", r.State)
	}
	var hs []headInfo
	json.Unmarshal(fx.run(sub.Runs[0].RunID).Heads, &hs)
	if len(hs) != 1 || hs[0].Merge != freshMerge || hs[0].SHA != head {
		t.Fatalf("tested %+v, want merge %s of head %s", hs, freshMerge, head)
	}
	// ... and the error it gave up with no longer leaves the head red.
	fx.runner.Wait()
	if ps := fx.forge.of("checks"); len(ps) == 0 || ps[len(ps)-1].SHA != head || ps[len(ps)-1].State != forge.StateSuccess {
		t.Fatalf("statuses on localci/checks = %+v, want the last one a success on the head", ps)
	}
}

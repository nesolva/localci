package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func stepCount(t *testing.T, yml string, ev Event) int {
	t.Helper()
	if ev.Name == "" {
		ev = Event{Name: "pull_request", Repo: "a/b", Branch: "main"}
	}
	res := PlanWorkflows("0123abc", ev, map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
	n := 0
	for _, c := range res.Checks {
		for _, s := range c.Steps {
			if strings.Contains(s.Run, "echo ran") {
				n++
			}
		}
	}
	return n
}

func TestTypedMatrixConditions(t *testing.T) {
	wf := func(vals, cond string) string {
		return "on: pull_request\njobs:\n  b:\n    runs-on: x\n    strategy:\n      matrix:\n        v: " + vals +
			"\n    steps:\n      - if: " + cond + "\n        run: echo ran\n"
	}
	for _, tc := range []struct {
		vals, cond string
		want       int
	}{
		{"[false]", "${{ !matrix.v }}", 1},
		{"[true]", "${{ !matrix.v }}", 0},
		{"[false]", "${{ matrix.v }}", 0},
		{"[0]", "${{ matrix.v }}", 0},
		{"[1]", "${{ matrix.v == 1 }}", 1},
		{"[true]", "${{ matrix.v == 'true' }}", 0}, // bool vs string: 1 vs NaN
		{"[18]", "${{ matrix.v == '18' }}", 1},     // number vs string coerces
		{"['false']", "${{ !matrix.v }}", 0},       // the string 'false' is truthy
		{"[1]", "${{ matrix.missing == null }}", 1},
	} {
		if got := stepCount(t, wf(tc.vals, tc.cond), Event{}); got != tc.want {
			t.Errorf("v: %s, if %s: ran %d steps, want %d", tc.vals, tc.cond, got, tc.want)
		}
	}
	job := "on: pull_request\njobs:\n  b:\n    if: ${{ !matrix.x }}\n    runs-on: x\n    strategy:\n      matrix:\n        x: [false]\n    steps:\n      - run: echo ran\n"
	if stepCount(t, job, Event{}) != 1 {
		t.Error("job if: !matrix.x with x=false must keep the job")
	}
	envWf := "on: pull_request\nenv:\n  F: 'false'\njobs:\n  b:\n    runs-on: x\n    steps:\n      - if: ${{ !env.F }}\n        run: echo ran\n"
	if stepCount(t, envWf, Event{}) != 0 {
		t.Error("!env.F with F='false' must be false")
	}
}

func TestBaseRefUnknownBranchRuns(t *testing.T) {
	wf := "on: pull_request\njobs:\n  b:\n    runs-on: x\n    steps:\n      - if: github.base_ref == 'main'\n        run: echo ran\n"
	if got := stepCount(t, wf, Event{Name: "pull_request", Repo: "a/b"}); got != 1 {
		t.Errorf("an unknown base branch must not drop the step, ran %d", got)
	}
	if got := stepCount(t, wf, Event{Name: "pull_request", Repo: "a/b", Branch: "dev"}); got != 0 {
		t.Errorf("base dev must drop the step, ran %d", got)
	}
}

func TestEmptyMatrixIsVisible(t *testing.T) {
	for name, m := range map[string]string{
		"empty axis":     "os: []",
		"excluded cells": "os: [a]\n        exclude:\n          - os: a",
	} {
		yml := "on: pull_request\njobs:\n  b:\n    runs-on: x\n    strategy:\n      matrix:\n        " + m + "\n    steps:\n      - run: echo ran\n"
		res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b"}, map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
		if len(res.Checks) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Job != "b" {
			t.Errorf("%s: want one visible skip for job b, got %+v", name, res)
		}
	}
}

func TestChangedFilesCountsRenamedAwayFiles(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("line of content\n", 50)
	if err := os.WriteFile(filepath.Join(dir, "src/a.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("mv", "src/a.go", "docs/a.go")
	run("commit", "-q", "-m", "move")
	files, err := ChangedFiles(dir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "docs/a.go,src/a.go" {
		t.Errorf("both sides of a rename must be listed, got %v", files)
	}
}

// A job whose needs were skipped is skipped too, unless its if: calls a
// status function; needs.<job>.result says what happened to a need.
func TestSkippedNeedsSkipTheirDependents(t *testing.T) {
	wf := `on: pull_request
jobs:
  publish:
    needs: release
    runs-on: x
    steps: [{run: echo publish}]
  release:
    if: github.event_name == 'push'
    runs-on: x
    steps: [{run: echo release}]
  announce:
    needs: [build, publish]
    runs-on: x
    steps: [{run: echo announce}]
  build:
    runs-on: x
    steps: [{run: echo build}]
  on-success:
    needs: release
    if: success()
    runs-on: x
    steps: [{run: echo on-success}]
  report:
    needs: release
    if: always()
    runs-on: x
    steps: [{run: echo report}]
  gate:
    needs: [build, release]
    if: ${{ !cancelled() && needs.release.result == 'skipped' && needs.build.result == 'success' }}
    runs-on: x
    steps: [{run: echo gate}]
`
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "acme/app", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(wf)})
	var ran []string
	for _, c := range res.Checks {
		ran = append(ran, c.Job)
	}
	if got := strings.Join(ran, ","); got != "build,report,gate" {
		t.Errorf("planned %s, want build,report,gate (in file order)", got)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Job] = s.Reason
	}
	for job, want := range map[string]string{"publish": "needs release", "announce": "needs publish", "on-success": "is false"} {
		if !strings.Contains(reasons[job], want) {
			t.Errorf("%s skipped with %q, want a reason with %q", job, reasons[job], want)
		}
	}
}

// A job whose if: cannot be decided before it runs is skipped with the reason,
// and so are the jobs that need it.
func TestUndecidedJobConditionSkipsTheJob(t *testing.T) {
	wf := `on: pull_request
jobs:
  deploy:
    if: vars.DEPLOY_ENABLED == 'true'
    runs-on: x
    steps: [{run: echo deploy}]
  notify:
    needs: deploy
    runs-on: x
    steps: [{run: echo notify}]
  test:
    runs-on: x
    steps: [{run: echo test}]
`
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "acme/app", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(wf)})
	if len(res.Checks) != 1 || res.Checks[0].Job != "test" {
		t.Fatalf("planned %+v, want only test", res.Checks)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Job] = s.Reason
	}
	if !strings.Contains(reasons["deploy"], "vars.DEPLOY_ENABLED") || !strings.Contains(reasons["notify"], "needs deploy") {
		t.Errorf("skips = %+v, want deploy (naming vars.DEPLOY_ENABLED) and notify (needs deploy)", res.Skipped)
	}
}

// A job that calls a reusable workflow is skipped like any other job when
// GitHub would skip it; only a job that would run is reported unsupported.
func TestReusableWorkflowJobFollowsItsConditions(t *testing.T) {
	wf := `on: pull_request
jobs:
  release:
    if: github.event_name == 'push'
    uses: acme/shared/.github/workflows/release.yml@main
  e2e:
    if: contains(github.event.pull_request.labels.*.name, 'e2e')
    uses: acme/shared/.github/workflows/e2e.yml@main
  docs:
    needs: release
    uses: acme/shared/.github/workflows/docs.yml@main
  lint:
    uses: acme/shared/.github/workflows/lint.yml@main
`
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "acme/app", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(wf)})
	if len(res.Checks) != 1 || res.Checks[0].Job != "lint" || !strings.Contains(res.Checks[0].Unsupported, "reusable workflow") {
		t.Errorf("planned %+v, want only lint, reported unsupported", res.Checks)
	}
	if len(res.Skipped) != 3 {
		t.Errorf("skipped %+v, want release, e2e and docs", res.Skipped)
	}
}

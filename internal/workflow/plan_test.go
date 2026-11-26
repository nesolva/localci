package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const keyWorkflow = `name: ci
on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: make test
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: make lint
`

func planOne(t *testing.T, repo, yml string) map[string]Check {
	t.Helper()
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: repo, Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
	out := map[string]Check{}
	for _, c := range res.Checks {
		out[c.Job] = c
	}
	return out
}

func TestKey(t *testing.T) {
	a := planOne(t, "acme/app", keyWorkflow)
	b := planOne(t, "acme/fork", keyWorkflow) // another checkout of the same definition
	if a["test"].Key == "" || a["test"].Key != b["test"].Key {
		t.Errorf("same definition must give the same key: %q vs %q", a["test"].Key, b["test"].Key)
	}
	if a["test"].Key == a["lint"].Key {
		t.Error("different jobs must not share a key")
	}

	c := planOne(t, "acme/app", strings.Replace(keyWorkflow, "make test", "make test-all", 1))
	if c["test"].Key == a["test"].Key {
		t.Error("a changed step must change the key")
	}
	if c["lint"].Key != a["lint"].Key {
		t.Error("changing one job must not change another job's key")
	}
}

func TestConditions(t *testing.T) {
	sc := &scope{event: Event{Name: "pull_request", Branch: "main"}, matrix: map[string]string{"os": "Linux"}}
	for _, tc := range []struct {
		expr          string
		run, always   bool
		wantErrSubstr string
	}{
		{expr: "github.event_name == 'pull_request'", run: true},
		{expr: "github.event_name == 'PULL_REQUEST'", run: true}, // GitHub compares case-insensitively
		{expr: "github.event_name != 'pull_request'", run: false},
		{expr: "failure()", run: false},
		{expr: "always()", run: true, always: true},
		{expr: "cancelled() || always()", run: true, always: true},
		{expr: "success() && github.event_name == 'push'", run: false},
		{expr: "!(github.event_name == 'push') && matrix.os == 'linux'", run: true},
		{expr: "github.event_name == 'push' || github.event_name == 'pull_request'", run: true},
		{expr: "${{ always() }}", run: true, always: true},
		// A side known at plan time decides && and || even when the other one is not.
		{expr: "github.event_name == 'push' && secrets.DEPLOY_KEY != ''", run: false},
		{expr: "secrets.DEPLOY_KEY != '' && github.event_name == 'push'", run: false},
		{expr: "github.event_name == 'pull_request' || steps.probe.outputs.hit", run: true},
		{expr: "failure() && steps.probe.outputs.hit == 'true'", run: false},
		{expr: "github.event_name == 'push' && contains(github.event.head_commit.message, '[release]')", run: false},
		{expr: "contains(github.event.pull_request.labels.*.name, 'deploy') && github.event_name == 'push'", run: false},
		{expr: "github.event_name == 'push' && fromJSON(vars.TARGETS)[0] == 'prod'", run: false},
		{expr: "!(github.event_name == 'push' && secrets.DEPLOY_KEY)", run: true},
		// String functions on known values; GitHub compares case-insensitively.
		{expr: "startsWith(github.event_name, 'PULL_')", run: true},
		{expr: "endsWith(matrix.os, 'UX')", run: true},
		{expr: "contains(github.event_name, 'push')", run: false},
		// Still unknown: nothing known decides it.
		{expr: "github.event_name == 'pull_request' && secrets.DEPLOY_KEY != ''", wantErrSubstr: "secrets.DEPLOY_KEY"},
		{expr: "secrets.DEPLOY_KEY || github.event_name == 'push'", wantErrSubstr: "secrets.DEPLOY_KEY"},
		{expr: "!steps.probe.outputs.hit", wantErrSubstr: "steps.probe.outputs.hit"},
		{expr: "contains(vars.TARGETS, 'prod')", wantErrSubstr: "vars.TARGETS"},
		{expr: "github.event_name == 'pull_request' && hashFiles('**/go.sum') != ''", wantErrSubstr: "hashFiles"},
		{expr: "github.event_name == 'push' && (", wantErrSubstr: "empty expression"},
	} {
		run, always, err := sc.condition(tc.expr)
		if tc.wantErrSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%q: want an unsupported error mentioning %q, got %v", tc.expr, tc.wantErrSubstr, err)
			}
			continue
		}
		if err != nil || run != tc.run || always != tc.always {
			t.Errorf("%q: got run=%v always=%v err=%v, want run=%v always=%v", tc.expr, run, always, err, tc.run, tc.always)
		}
	}
}

// The github context of the event is known at plan time.
func TestGithubContextConditions(t *testing.T) {
	pr := Event{Name: "pull_request", Repo: "acme/app", Branch: "main", PR: 7, HeadBranch: "feature/x"}
	noNumber := Event{Name: "pull_request", Repo: "acme/app", Branch: "main"} // localci plan, a working tree
	push := Event{Name: "push", Repo: "acme/app", Branch: "main"}
	for _, tc := range []struct {
		ev            Event
		expr          string
		run           bool
		wantErrSubstr string
	}{
		{ev: pr, expr: "github.ref == 'refs/pull/7/merge'", run: true},
		{ev: pr, expr: "github.ref == 'refs/heads/main'", run: false},
		{ev: pr, expr: "github.event_name == 'push' || github.ref == 'refs/heads/main'", run: false},
		{ev: pr, expr: "startsWith(github.ref, 'refs/tags/')", run: false},
		{ev: pr, expr: "github.ref_type == 'tag'", run: false},
		{ev: pr, expr: "github.ref_name == '7/merge'", run: true},
		{ev: pr, expr: "startsWith(github.head_ref, 'feature/')", run: true},
		{ev: pr, expr: "github.repository == 'acme/app' && github.repository_owner == 'acme'", run: true},
		{ev: noNumber, expr: "github.ref == 'refs/heads/main'", run: false},
		{ev: noNumber, expr: "github.ref != 'refs/heads/main'", run: true},
		{ev: noNumber, expr: "github.head_ref == 'feature/x'", wantErrSubstr: "github.head_ref"},
		{ev: push, expr: "github.ref == 'refs/heads/main' && github.ref_name == 'main'", run: true},
		{ev: push, expr: "github.head_ref == ''", run: true},
	} {
		sc := &scope{event: tc.ev}
		run, _, err := sc.condition(tc.expr)
		if tc.wantErrSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("%s %q: want an unsupported error mentioning %q, got run=%v err=%v", tc.ev.Name, tc.expr, tc.wantErrSubstr, run, err)
			}
			continue
		}
		if err != nil || run != tc.run {
			t.Errorf("%s #%d %q: got run=%v err=%v, want run=%v", tc.ev.Name, tc.ev.PR, tc.expr, run, err, tc.run)
		}
	}

	// In text, a ref without its pull request number is not guessed.
	if out, bad := (&scope{event: pr}).subst("${{ github.ref }}"); out != "refs/pull/7/merge" || bad != nil {
		t.Errorf("github.ref with a number = %q %v", out, bad)
	}
	if out, bad := (&scope{event: noNumber}).subst("${{ github.ref }}"); out != "${{ github.ref }}" || len(bad) != 1 {
		t.Errorf("github.ref without a number = %q %v, want it left unresolved", out, bad)
	}
}

func TestDockerTags(t *testing.T) {
	got := dockerTags("sudo docker build --tag=acme/a:1 .\ndocker run --rm -p 1:1 -e A=b acme/a:1 test && docker run busybox\ndocker build -t acme/$X .")
	want := []string{"acme/a:1", "busybox"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestPlanFromGit reads workflows from the commit (not the working tree) and
// takes the changed files from the diff against the base branch.
func TestPlanFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p, content string) {
		t.Helper()
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write(".github/workflows/build.yml", "name: build\non:\n  pull_request:\n    paths-ignore: ['docs/**']\njobs:\n  b:\n    runs-on: x\n    steps:\n      - run: make\n")
	write("src/a.go", "package a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	write("docs/readme.md", "docs\n")
	run("add", "-A")
	run("commit", "-q", "-m", "docs only")
	// An uncommitted edit of the workflow must not matter.
	write(".github/workflows/build.yml", "name: other\non: push\njobs: {}\n")

	res, err := PlanDetailed(dir, "HEAD", Event{Name: "pull_request", Branch: "main", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Checks) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Workflow != "build" || !strings.Contains(res.Skipped[0].Reason, "paths-ignore") {
		t.Errorf("docs-only change should skip the build workflow, got %+v", res)
	}

	write("src/b.go", "package a\n")
	run("add", "src")
	run("commit", "-q", "-m", "code")
	checks, err := Plan(dir, "HEAD", Event{Name: "pull_request", Branch: "main", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 || checks[0].Job != "b" || checks[0].Repo != filepath.Base(dir) {
		t.Errorf("a code change should plan the build job, got %+v", checks)
	}

	if _, err := Plan(dir, "nosuchref", Event{Name: "pull_request"}); err == nil {
		t.Error("an unknown commit must be an error")
	}
}

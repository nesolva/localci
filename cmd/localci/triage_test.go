package main

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/triage"
)

func triageReport() server.TriageReport {
	return server.TriageReport{Run: failedRun(), Propose: "patch", Tests: []server.TriageTest{
		{Test: "t1", Title: "checkout › pays with saved card", File: "tests/cart.spec.ts", Line: 41, Status: "failed", Class: "product-bug",
			Lines: []string{"assertion failed: expected \"Thank you\", got \"Something went wrong\"", "network: POST /api/payments answered 500", "console: TypeError: card is undefined (app.js:210)"}},
		{Test: "t2", Title: "cart › removes a line", File: "tests/cart.spec.ts", Line: 77, Status: "failed", Class: "test-bug",
			Lines: []string{`locator getByRole('button', { name: 'Remove' }) matches nothing`, `the page has button "Delete" (edit distance 3)`,
				`fix: replace "Remove" with "Delete" in tests/cart.spec.ts (localci triage 42 --apply)`}},
		{Test: "t3", Title: "catalog › filters by tag", File: "tests/catalog.spec.ts", Line: 12, Status: "passed", Class: "flaky",
			Note: "failed on the way, passed in the end: the run is green", Lines: []string{"failed 1, passed 1 of 2 attempts on the same tree"}},
		{Test: "t4", Title: "auth › signs in", File: "tests/auth.spec.ts", Line: 9, Status: "failed", Class: "infrastructure",
			Lines: []string{"connection refused to the stack: ERR_CONNECTION_REFUSED", "the stack or the test command ended before the test finished"}},
		{Test: "t5", Title: "settings › saves", File: "tests/settings.spec.ts", Line: 30, Status: "failed", Class: "unknown",
			Lines: []string{"no 5xx, console error or near match on the page"}},
	}}
}

func TestTriageGoldenShowsEveryClassWithAtMostThreeEvidenceLines(t *testing.T) {
	url := fake(t, map[string]any{"GET /api/runs/42/triage": triageReport()})
	out, _, code := cli(t, url, "triage", "42")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	golden(t, "triage.txt", out)

	var evidence int
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(l, "  ") {
			evidence++
			continue
		}
		if evidence > 3 {
			t.Errorf("a test printed %d evidence lines", evidence)
		}
		evidence = 0
	}
}

func TestTriageTestFilterAndNothingToTriage(t *testing.T) {
	rep := triageReport()
	url := fake(t, map[string]any{"GET /api/runs/42/triage": rep})
	out, _, code := cli(t, url, "triage", "42", "--test", "removes")
	if code != 0 || !strings.HasPrefix(out, "test-bug · cart › removes a line") || strings.Contains(out, "product-bug") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, errOut, code := cli(t, url, "triage", "42", "--test", "nope"); code != 1 || !strings.Contains(errOut, "no classified test matches") {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	rep.Tests = nil
	rep.Run.State = "passed"
	url = fake(t, map[string]any{"GET /api/runs/42/triage": rep})
	if out, _, code := cli(t, url, "triage", "42"); code != 0 || !strings.Contains(out, "nothing to triage") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

const cartSpec = `import { test } from "./fixtures";

test("removes a line", async ({ page }) => {
  await page.getByRole("button", { name: "Remove" }).click();
  await page.getByRole("button", { name: "Save" }).click();
});

test("empties the cart", async ({ page }) => {
  await page.goto("/cart");
  await page.getByRole("link", { name: "Cart" }).click();
  await page.getByRole("button", { name: "Empty" }).click();
});
`

// patched makes a checkout holding the spec and the report of a run whose test bug has its patch.
func patched(t *testing.T, fork bool, propose string) (repo string, rep server.TriageReport) {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn := func(dir string, args ...string) string {
		cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitIn(repo, "init", "-q", "-b", "main")
	os.MkdirAll(filepath.Join(repo, "tests"), 0o755)
	os.WriteFile(filepath.Join(repo, "tests/cart.spec.ts"), []byte(cartSpec), 0o644)
	gitIn(repo, "add", "-A")
	gitIn(repo, "commit", "-qm", "init")
	head := gitIn(repo, "rev-parse", "HEAD")

	p, why := triage.BuildPatch(repo, filepath.Join(repo, "tests/cart.spec.ts")+":4", []string{"cart.spec.ts"},
		triage.Fix{Method: "getByRole", Role: "button", From: "Remove", To: "Delete"})
	if p == nil {
		t.Fatal(why)
	}
	file := filepath.Join(t.TempDir(), "t2.patch")
	os.WriteFile(file, []byte(p.Diff), 0o644)

	rep = triageReport()
	rep.Fork, rep.Propose = fork, propose
	rep.Tests = rep.Tests[1:2]
	rep.Tests[0].Patch = &triage.PatchInfo{Path: file, File: p.File, Shas: []string{head}, From: "Remove", To: "Delete"}
	return repo, rep
}

func TestTriageApplyChangesTheWorkingTreeOnly(t *testing.T) {
	repo, rep := patched(t, false, "patch")
	t.Chdir(repo)
	url := fake(t, map[string]any{"GET /api/runs/42/triage": rep})
	out, errOut, code := cli(t, url, "triage", "42", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, `applied: "Remove" → "Delete" in tests/cart.spec.ts`) {
		t.Fatalf("output:\n%s", out)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "tests/cart.spec.ts"))
	if !strings.Contains(string(got), `name: "Delete"`) {
		t.Fatalf("file:\n%s", got)
	}
	if st := gitStatus(t, repo); st != "M tests/cart.spec.ts" {
		t.Fatalf("status %q: only the working tree changes", st)
	}
}

// addFix gives the run one more failed test, whose patch replaces from with to in the locator on that line of the spec.
func addFix(t *testing.T, repo string, rep *server.TriageReport, test string, line int, from, to string) {
	t.Helper()
	p, why := triage.BuildPatch(repo, filepath.Join(repo, "tests/cart.spec.ts")+":"+strconv.Itoa(line), []string{"cart.spec.ts"},
		triage.Fix{Method: "getByRole", Role: "button", From: from, To: to})
	if p == nil {
		t.Fatal(why)
	}
	file := filepath.Join(t.TempDir(), test+".patch")
	if err := os.WriteFile(file, []byte(p.Diff), 0o644); err != nil {
		t.Fatal(err)
	}
	tt := rep.Tests[0]
	tt.Test = test
	tt.Patch = &triage.PatchInfo{Path: file, File: p.File, Shas: rep.Tests[0].Patch.Shas, From: from, To: to}
	rep.Tests = append(rep.Tests, tt)
}

// One spec run under two projects, or a broken locator in a shared beforeEach, gives two failed tests the same
// patch: it is applied once, and the command succeeds.
func TestTriageApplyAppliesAPatchTwoTestsShareOnce(t *testing.T) {
	repo, rep := patched(t, false, "patch")
	addFix(t, repo, &rep, "t2-firefox", 4, "Remove", "Delete")
	t.Chdir(repo)
	out, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--apply")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if n := strings.Count(out, `applied: "Remove" → "Delete" in tests/cart.spec.ts`); n != 1 {
		t.Fatalf("the shared fix is reported %d times:\n%s", n, out)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "tests/cart.spec.ts"))
	if !strings.Contains(string(got), `name: "Delete"`) {
		t.Fatalf("file:\n%s", got)
	}
}

// Fixes are applied all together or not at all: two that change neighbouring lines do not fit one after the other,
// and the working tree must not keep the first one alone.
func TestTriageApplyIsAllOrNothing(t *testing.T) {
	repo, rep := patched(t, false, "patch")
	addFix(t, repo, &rep, "t6", 5, "Save", "Store")
	t.Chdir(repo)
	_, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--apply")
	if code != 1 || !strings.HasPrefix(errOut, "localci: not applied:") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if st := gitStatus(t, repo); st != "" {
		t.Fatalf("status %q: a refused set of fixes left the working tree half-patched", st)
	}

	// Fixes apart from each other all apply.
	repo, rep = patched(t, false, "patch")
	addFix(t, repo, &rep, "t8", 11, "Empty", "Clear")
	t.Chdir(repo)
	out, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--apply")
	if code != 0 || strings.Count(out, "applied: ") != 2 {
		t.Fatalf("exit %d: %s\n%s", code, errOut, out)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "tests/cart.spec.ts"))
	if !strings.Contains(string(got), `name: "Delete"`) || !strings.Contains(string(got), `name: "Clear"`) {
		t.Fatalf("file:\n%s", got)
	}
}

func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	cmd := osexec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func TestTriageApplyRefusals(t *testing.T) {
	t.Run("a different head", func(t *testing.T) {
		repo, rep := patched(t, false, "patch")
		rep.Tests[0].Patch.Shas = []string{strings.Repeat("b", 40)}
		t.Chdir(repo)
		_, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--apply")
		if code != 1 || !strings.Contains(errOut, "the run tested bbbbbbbb") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if gitStatus(t, repo) != "" {
			t.Fatal("the working tree changed on a refusal")
		}
	})
	t.Run("a fork's pull request", func(t *testing.T) {
		repo, rep := patched(t, true, "patch")
		t.Chdir(repo)
		_, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--apply")
		if code != 1 || !strings.Contains(errOut, "fork") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if gitStatus(t, repo) != "" {
			t.Fatal("the working tree changed on a refusal")
		}
	})
}

// fakeGH puts a `gh` on PATH that records its arguments.
func fakeGH(t *testing.T) (record string) {
	t.Helper()
	bin := t.TempDir()
	record = filepath.Join(t.TempDir(), "gh.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\necho https://example.test/acme/shop/pull/7\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func TestTriagePROnlyWhenTheSuiteProposesIt(t *testing.T) {
	t.Run("refuses by default", func(t *testing.T) {
		record := fakeGH(t)
		repo, rep := patched(t, false, "patch")
		t.Chdir(repo)
		_, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--pr")
		if code != 1 || !strings.Contains(errOut, "triage.propose: pr") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(record); err == nil {
			t.Fatal("gh was called")
		}
	})
	t.Run("refuses a fork", func(t *testing.T) {
		record := fakeGH(t)
		repo, rep := patched(t, true, "pr")
		t.Chdir(repo)
		_, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--pr")
		if code != 1 || !strings.Contains(errOut, "fork") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(record); err == nil {
			t.Fatal("gh was called")
		}
	})
	t.Run("pushes a branch and calls gh with it", func(t *testing.T) {
		record := fakeGH(t)
		repo, rep := patched(t, false, "pr")
		remote := t.TempDir()
		if out, err := osexec.Command("git", "init", "-q", "--bare", remote).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, args := range [][]string{{"remote", "add", "origin", remote}} {
			cmd := osexec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
		t.Chdir(repo)
		out, errOut, code := cli(t, fake(t, map[string]any{"GET /api/runs/42/triage": rep}), "triage", "42", "--pr")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if !strings.Contains(out, "https://example.test/acme/shop/pull/7") {
			t.Fatalf("output:\n%s", out)
		}
		args, _ := os.ReadFile(record)
		branch := "localci/fix-t2-run42"
		for _, want := range []string{"pr", "create", "--head", branch, "--title", `fix(test): "Remove" → "Delete" in cart › removes a line`} {
			if !strings.Contains(string(args), want+"\n") {
				t.Errorf("gh args lack %q:\n%s", want, args)
			}
		}
		cmd := osexec.Command("git", "--git-dir", remote, "rev-parse", "--verify", branch)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("branch not pushed: %v\n%s", err, out)
		}
		if gitStatus(t, repo) != "" {
			t.Fatal("the checkout changed")
		}
		if wt, _ := osexec.Command("git", "-C", repo, "worktree", "list").Output(); strings.Count(string(wt), "\n") != 1 {
			t.Fatalf("the throwaway worktree is still there:\n%s", wt)
		}
	})
}

func TestRunsFlakyListsTheCounts(t *testing.T) {
	url := fake(t, map[string]any{"GET /api/flaky?days=30": server.FlakyList{Days: 30, Tests: nil}})
	out, _, code := cli(t, url, "runs", "--flaky")
	if code != 0 || !strings.Contains(out, "no flaky tests in the last 30 days") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Titles, files and evidence come from a report the code under test wrote: a terminal escape in them must reach the
// agent's terminal as harmless text, never as a command (clear the screen, a hidden hyperlink, a reversed line).
func TestTriageOutputCarriesNoTerminalEscapes(t *testing.T) {
	rep := triageReport()
	rep.Tests[0].Title = "pays \x1b]8;;https://evil.test\x07here\x1b]8;;\x07"
	rep.Tests[0].File = "tests/\x1b[2Jcart.spec.ts"
	rep.Tests[0].Lines = []string{"expected \x1b[31mred\x1b[0m \u202egnirts\u202c \x9b2J"}
	url := fake(t, map[string]any{"GET /api/runs/42/triage": rep})
	out, _, code := cli(t, url, "triage", "42")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if i := strings.IndexFunc(out, func(r rune) bool {
		return r != '\n' && (unicode.IsControl(r) || r == '\u202e' || r == '\u202c')
	}); i >= 0 {
		t.Fatalf("triage printed a control character %q:\n%q", out[i:i+1], out)
	}
	if !strings.Contains(out, "pays") || !strings.Contains(out, "red") {
		t.Fatalf("the visible text is gone:\n%s", out)
	}
}

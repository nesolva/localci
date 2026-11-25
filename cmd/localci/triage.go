package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/triage"
)

func init() {
	register("triage", "why each failed test failed (product bug, test bug, flaky, infrastructure), with a patch for a broken selector", true,
		agent("triage", "<id> [--test id] [--apply] [--pr]", func(fs *flag.FlagSet) {
			fs.String("test", "", "only this test (its id, or part of its title)")
			fs.Bool("apply", false, "apply the proposed selector fixes to this checkout's working tree")
			fs.Bool("pr", false, "open a pull request with the fix (needs `triage.propose: pr` in the suite)")
		}, cmdTriage))
}

func cmdTriage(e *env, args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(e.err, "usage: localci triage <id> [--test id] [--apply] [--pr]")
		return exitUsage
	}
	id := args[0]
	apply, pr := flagOn(e, "apply"), flagOn(e, "pr")
	if apply && pr {
		fmt.Fprintln(e.err, "localci: --apply and --pr are two ways to use a fix: choose one")
		return exitUsage
	}
	var rep server.TriageReport
	if err := e.c.get("/api/runs/"+url.PathEscape(id)+"/triage", &rep); err != nil {
		return e.fail(err)
	}
	tests := rep.Tests
	if want := e.fs.Lookup("test").Value.String(); want != "" {
		tests = nil
		for _, t := range rep.Tests {
			if t.Test == want || strings.Contains(strings.ToLower(t.Title), strings.ToLower(want)) {
				tests = append(tests, t)
			}
		}
		if len(tests) == 0 {
			fmt.Fprintf(e.err, "localci: no classified test matches %q in run %s\n", want, id)
			return exitFailed
		}
	}

	if e.json {
		for _, t := range tests {
			e.emit("triage", t)
		}
	} else {
		printTriage(e, id, rep, tests)
	}
	switch {
	case apply:
		return applyFixes(e, id, rep, tests)
	case pr:
		return openPR(e, id, rep, tests)
	}
	return exitOK
}

// printTriage writes one block per test: its class, title and place, then the evidence (three lines at most).
func printTriage(e *env, id string, rep server.TriageReport, tests []server.TriageTest) {
	if len(tests) == 0 {
		fmt.Fprintf(e.out, "run %s: nothing to triage, no test failed (%s)\n", id, rep.Run.State)
		return
	}
	for _, t := range tests {
		where := printable(t.File)
		if t.Line > 0 {
			where += fmt.Sprintf(":%d", t.Line)
		}
		head := fmt.Sprintf("%s · %s · %s", t.Class, trim(t.Title, 80), where)
		if t.Note != "" {
			head += " · " + trim(t.Note, 80)
		}
		fmt.Fprintln(e.out, head)
		for _, l := range t.Lines[:min(len(t.Lines), 3)] {
			fmt.Fprintln(e.out, "  "+trim(l, 200))
		}
	}
}

// withPatch is the tests that carry a fix.
func withPatch(tests []server.TriageTest) []server.TriageTest {
	var out []server.TriageTest
	for _, t := range tests {
		if t.Patch != nil {
			out = append(out, t)
		}
	}
	return out
}

func applyFixes(e *env, id string, rep server.TriageReport, tests []server.TriageTest) int {
	fixes := withPatch(tests)
	if len(fixes) == 0 {
		fmt.Fprintf(e.err, "localci: run %s has no proposed fix to apply\n", id)
		return exitFailed
	}
	// Every patch is checked before any is applied, and they go in one `git apply`, which changes nothing unless
	// all of them fit. Tests can share a patch (one spec under two projects, a locator in a beforeEach): once is enough.
	ctx := context.Background()
	cwd, _ := os.Getwd()
	var root string
	var all bytes.Buffer
	var applied []server.TriageTest
	seen := map[string]bool{}
	for _, t := range fixes {
		diff, r, err := triage.Check(ctx, cwd, *t.Patch, rep.Fork)
		if err != nil {
			return refuse(e, err)
		}
		if seen[string(diff)] {
			continue
		}
		seen[string(diff)] = true
		root = r
		all.Write(diff)
		if !bytes.HasSuffix(diff, []byte("\n")) {
			all.WriteByte('\n')
		}
		applied = append(applied, t)
	}
	if err := gitApply(ctx, root, all.Bytes()); err != nil {
		if len(applied) > 1 {
			err = triage.Refusal(fmt.Sprintf("the %d fixes overlap and do not apply together: apply one at a time with --test <id>",
				len(applied)))
		}
		return refuse(e, err)
	}
	for _, t := range applied {
		fmt.Fprintf(e.out, "applied: %s → %s in %s (working tree only, nothing committed)\n",
			quoteText(t.Patch.From), quoteText(t.Patch.To), printable(t.Patch.File))
	}
	return exitOK
}

// gitApply applies diff to the working tree of the checkout at root; it changes nothing when any part fails.
func gitApply(ctx context.Context, root string, diff []byte) error {
	cmd := osexec.CommandContext(ctx, "git", "apply", "-")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdin = bytes.NewReader(diff)
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git apply: %s", strings.TrimSpace(b.String()))
	}
	return nil
}

func refuse(e *env, err error) int {
	var r triage.Refusal
	if errors.As(err, &r) {
		fmt.Fprintln(e.err, "localci: not applied:", r)
	} else {
		fmt.Fprintln(e.err, "localci:", err)
	}
	return exitFailed
}

func quoteText(s string) string { return `"` + strings.ReplaceAll(printable(s), `"`, `\"`) + `"` }

// openPR commits the fix of one test on a branch of its own, in a throwaway worktree, pushes it and asks `gh` for
// the pull request. It touches neither the current branch nor the working tree.
func openPR(e *env, id string, rep server.TriageReport, tests []server.TriageTest) int {
	if rep.Propose != "pr" {
		fmt.Fprintf(e.err, "localci: not opened: suite %s does not allow it; set suites.%s.triage.propose: pr, or use --apply\n",
			rep.Run.Suite, rep.Run.Suite)
		return exitFailed
	}
	if rep.Fork {
		return refuse(e, triage.Refusal("this run tests a fork's pull request: its patches never leave the machine automatically"))
	}
	fixes := withPatch(tests)
	switch {
	case len(fixes) == 0:
		fmt.Fprintf(e.err, "localci: run %s has no proposed fix to open a pull request for\n", id)
		return exitFailed
	case len(fixes) > 1:
		fmt.Fprintf(e.err, "localci: %d tests have a fix: choose one with --test\n", len(fixes))
		return exitFailed
	}
	t := fixes[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cwd, _ := os.Getwd()
	if _, root, err := triage.Check(ctx, cwd, *t.Patch, false); err != nil {
		return refuse(e, err)
	} else {
		cwd = root
	}

	short := t.Test
	if len(short) > 8 {
		short = short[:8]
	}
	branch := fmt.Sprintf("localci/fix-%s-run%s", branchSafe(short), branchSafe(id))
	tmp, err := os.MkdirTemp("", "localci-pr-")
	if err != nil {
		return e.fail(err)
	}
	wt := filepath.Join(tmp, "wt")
	defer os.RemoveAll(tmp)
	if out, err := sh(ctx, cwd, "git", "worktree", "add", "-q", "-b", branch, wt, "HEAD"); err != nil {
		fmt.Fprintf(e.err, "localci: cannot make a branch for the fix: %s\n", out)
		return exitFailed
	}
	defer func() {
		sh(context.Background(), cwd, "git", "worktree", "remove", "--force", wt)
	}()
	if err := triage.Apply(ctx, wt, *t.Patch, false); err != nil {
		return refuse(e, err)
	}
	msg := fmt.Sprintf("fix(test): %s → %s in %s", quoteText(t.Patch.From), quoteText(t.Patch.To), trim(t.Title, 60))
	for _, step := range [][]string{
		{"git", "add", "--", t.Patch.File},
		{"git", "commit", "-q", "-m", msg, "-m", "Proposed by localci triage of run " + id + ".\n\n" + strings.Join(t.Lines, "\n")},
		{"git", "push", "-q", "-u", "origin", branch},
	} {
		if out, err := sh(ctx, wt, step[0], step[1:]...); err != nil {
			fmt.Fprintf(e.err, "localci: %s failed: %s\n", strings.Join(step[:2], " "), out)
			return exitFailed
		}
	}
	body := fmt.Sprintf("localci run %s: %s\n\n%s\n\nThe fix changes one locator in %s. Review it before merging.",
		id, t.Class, strings.Join(t.Lines, "\n"), t.Patch.File)
	out, err := sh(ctx, wt, "gh", "pr", "create", "--head", branch, "--title", msg, "--body", body)
	if err != nil {
		fmt.Fprintf(e.err, "localci: gh pr create failed (the branch %s is pushed): %s\n", branch, out)
		return exitFailed
	}
	fmt.Fprintf(e.out, "pull request opened from %s: %s\n", branch, lastLine(out))
	return exitOK
}

func sh(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	return strings.TrimSpace(b.String()), err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// branchSafe keeps a name to what a branch can hold without quoting.
func branchSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// cmdFlaky lists the tests classed flaky over the last 30 days, most frequent first.
func cmdFlaky(e *env) int {
	var l server.FlakyList
	if err := e.c.get("/api/flaky?days=30", &l); err != nil {
		return e.fail(err)
	}
	for _, t := range l.Tests {
		if e.json {
			e.emit("flaky", t)
			continue
		}
		where := t.File
		fmt.Fprintf(e.out, "%d runs · %s · %s\n", t.Runs, trim(t.Title, 80), where)
	}
	if len(l.Tests) == 0 && !e.json {
		fmt.Fprintf(e.out, "no flaky tests in the last %d days\n", l.Days)
	}
	return exitOK
}

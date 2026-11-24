package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeActBin puts an `act` that runs script on PATH.
func fakeActBin(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "act"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func actSpecIn(t *testing.T) ActSpec {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte("name: CI\njobs:\n  t: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ActSpec{Dir: dir, Workflow: "CI", Job: "t", Matrix: map[string]string{"node": "3.10"},
		Repo: "o/r", Base: "main", Head: "abc", Merge: "def", PR: 1, LogPath: filepath.Join(t.TempDir(), "run.log")}
}

// act exits 0 without running anything when no matrix cell matches or the job
// is skipped; that must not count as a pass.
func TestActExitZeroWithoutAJobRunIsNotASuccess(t *testing.T) {
	fakeActBin(t, `echo "level=warning msg=nothing matched"; exit 0`)
	_, err := RunAct(context.Background(), actSpecIn(t))
	if !errors.Is(err, ErrActRanNothing) || !strings.Contains(err.Error(), "node:3.1") {
		t.Fatalf("err = %v, want ErrActRanNothing naming the matrix cell", err)
	}
}

func TestActJobSucceededMarkerIsASuccess(t *testing.T) {
	fakeActBin(t, `echo "[CI/t] Job succeeded"; exit 0`)
	res, err := RunAct(context.Background(), actSpecIn(t))
	if err != nil || !res.OK() {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}

// act compares matrix values as parsed YAML, so 3.10 only matches as 3.1.
func TestActMatrixValuesAreNormalisedLikeYAML(t *testing.T) {
	for in, want := range map[string]string{"3.10": "3.1", "3.9": "3.9", "18": "18", "ubuntu": "ubuntu", "1.0": "1", "v3.10": "v3.10"} {
		if got := actMatrixValue(in); got != want {
			t.Errorf("actMatrixValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActTimeoutRemovesItsContainersByLabel(t *testing.T) {
	spec := actSpecIn(t)
	spec.Timeout = 300 * time.Millisecond
	fakeActBin(t, `sleep 30`)
	cleaned := ""
	removeLabeled = func(ctx context.Context, label string) { cleaned = label }
	t.Cleanup(func() { removeLabeled = RemoveLabeled })
	res, _ := RunAct(context.Background(), spec)
	if !res.TimedOut || !strings.HasPrefix(cleaned, "localci.act=") {
		t.Fatalf("timedOut=%v cleaned=%q", res.TimedOut, cleaned)
	}
}

// A PR can commit anything under its own tree. act must run with a working directory of ours and with every
// option that has a file default pointing at an empty file, whatever the PR put in .actrc, .secrets, .env.
func TestActIgnoresConfigFilesCommittedInThePullRequest(t *testing.T) {
	spec := actSpecIn(t)
	spec.Run = "r1"
	for name, body := range map[string]string{
		".actrc":   "--privileged\n-v /:/host\n",
		".secrets": "TOKEN=stolen\n",
		".env":     "X=1\n",
		".vars":    "V=1\n",
		".input":   "I=1\n",
	} {
		if err := os.WriteFile(filepath.Join(spec.Dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), "seen")
	// The fake act records what a real one would act on, then reads the files like act does.
	fakeActBin(t, `pwd -P > "`+out+`"; printf '%s\n' "$@" >> "`+out+`"; env >> "`+out+`"; echo "[CI/t] Job succeeded"`)
	if _, err := RunAct(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	seen := string(b)
	first, _, _ := strings.Cut(seen, "\n")
	wantCwd, _ := filepath.EvalSymlinks(filepath.Join(filepath.Dir(spec.LogPath), "act-cwd"))
	if first != wantCwd {
		t.Errorf("act ran in %q, want a directory of ours (%q)", first, wantCwd)
	}
	for _, leak := range []string{"--privileged", "-v /:/host", "stolen", "X=1"} {
		if strings.Contains(seen, leak) {
			t.Errorf("act saw %q from the workspace:\n%s", leak, seen)
		}
	}
	args := strings.Fields(seen)
	for _, flag := range []string{"--secret-file", "--env-file", "--var-file", "--input-file"} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("%s is not passed explicitly: %v", flag, args)
		}
		if b, err := os.ReadFile(args[i+1]); err != nil || len(b) != 0 {
			t.Errorf("%s points at %q, want an empty file", flag, args[i+1])
		}
	}
	if !strings.Contains(seen, "localci.run=r1") {
		t.Errorf("job container is not labelled with its run:\n%s", seen)
	}
}

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
)

// openAdmit lets every check start at once.
type openAdmit struct{}

func (openAdmit) Admit(context.Context, checks.Cost) (func(), error) { return func() {}, nil }

const threeChecks = `name: CI
on: [pull_request]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo linted
  unit:
    runs-on: ubuntu-latest
    steps:
      - run: echo tested
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo built
`

// checksRig is a repository with pull request 1 and a checks runner reading its workflows.
type checksRig struct {
	t      *testing.T
	src    string
	head   string
	st     *store.Store
	fg     *recordForge
	runner *checks.Runner
	srv    *Server
}

func newChecksRig(t *testing.T) *checksRig {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	rg := &checksRig{t: t, src: t.TempDir(), st: openStore(t), fg: &recordForge{}}
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rg.src, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.src, ".github", "workflows", "ci.yml"), []byte(threeChecks), 0o644); err != nil {
		t.Fatal(err)
	}
	rg.git("init", "-q", "-b", "main")
	rg.git("add", "-A")
	rg.git("commit", "-q", "-m", "base")
	rg.head = rg.git("rev-parse", "HEAD")
	rg.git("update-ref", "refs/pull/1/head", rg.head)
	rg.runner = &checks.Runner{Store: rg.st, Forge: rg.fg, Admit: openAdmit{}, Options: checks.Options{DataDir: data, Toolchains: exec.ToolchainContainers},
		Mirror: func(ctx context.Context, repo string) (*exec.Mirror, error) {
			return exec.EnsureMirror(ctx, data, repo, rg.src)
		}}
	t.Cleanup(rg.runner.Wait)
	rg.srv = New(Deps{Store: rg.st, Checks: rg.runner})
	return rg
}

func (rg *checksRig) git(args ...string) string {
	rg.t.Helper()
	out, err := osexec.Command("git", append([]string{"-C", rg.src, "-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput()
	if err != nil {
		rg.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// submitAll runs the pull request's checks and waits for them.
func (rg *checksRig) submitAll() checks.Submission {
	rg.t.Helper()
	sub, err := rg.runner.Submit(context.Background(), checks.Request{Repo: "acme/web", PR: 1, Base: "main", Head: rg.head, Trigger: "push"})
	if err != nil {
		rg.t.Fatal(err)
	}
	for _, r := range sub.Runs {
		select {
		case <-r.Done:
		case <-time.After(60 * time.Second):
			rg.t.Fatalf("run %s did not finish", r.RunID)
		}
	}
	return sub
}

func (rg *checksRig) checkRuns() int {
	rg.t.Helper()
	runs, err := rg.st.Runs(context.Background(), 0)
	if err != nil {
		rg.t.Fatal(err)
	}
	n := 0
	for _, r := range runs {
		if r.Kind == store.KindCheck {
			n++
		}
	}
	return n
}

func TestRetryCheckRerunsOnlyThatCheck(t *testing.T) {
	rg := newChecksRig(t)
	sub := rg.submitAll()
	if len(sub.Runs) != 3 {
		t.Fatalf("submitted %d checks, want 3", len(sub.Runs))
	}
	var unit store.Run
	for _, s := range sub.Runs {
		if s.Name == "unit" {
			unit, _ = rg.st.Run(t.Context(), s.RunID)
		}
	}
	runsBefore, postsBefore := rg.checkRuns(), len(rg.fg.statuses())

	res, err := rg.srv.retryCheck(t.Context(), unit)
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	rg.runner.Wait()

	if got := rg.checkRuns() - runsBefore; got != 1 {
		t.Errorf("the retry started %d check runs, want 1", got)
	}
	reran := false
	for _, p := range rg.fg.statuses()[postsBefore:] {
		switch p.st.Suite {
		case "CI/unit":
			reran = true
		case "checks": // the summary of the head, which every retry updates
		default:
			t.Errorf("the retry posted a status for %q", p.st.Suite)
		}
	}
	if !reran {
		t.Error("the retry posted nothing for the check it reran")
	}
}

func TestRetryCheckHeadMoved(t *testing.T) {
	rg := newChecksRig(t)
	sub := rg.submitAll()
	old, _ := rg.st.Run(t.Context(), sub.Runs[0].RunID)

	rg.git("commit", "-q", "--allow-empty", "-m", "newer")
	moved := rg.git("rev-parse", "HEAD")
	rg.git("update-ref", "refs/pull/1/head", moved)

	_, err := rg.srv.retryCheck(t.Context(), old)
	var he *httpError
	if !errors.As(err, &he) || he.status != http.StatusConflict {
		t.Fatalf("retry after the head moved = %v, want a 409", err)
	}
	if !strings.Contains(he.msg, moved[:7]) {
		t.Errorf("message %q does not name the new head %s", he.msg, moved[:7])
	}
}

// A check whose name got a matrix suffix (two cells shared a name) is found again on retry, and only it reruns.
func TestRetryCheckFindsASuffixedName(t *testing.T) {
	rg := newChecksRig(t)
	wf := `name: CI
on: [pull_request]
jobs:
  test:
    name: test ${{ matrix.os }}
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux]
        node: [18, 20]
    steps:
      - run: echo ${{ matrix.node }}
`
	if err := os.WriteFile(filepath.Join(rg.src, ".github", "workflows", "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	rg.git("add", "-A")
	rg.git("commit", "-q", "-m", "matrix")
	rg.head = rg.git("rev-parse", "HEAD")
	rg.git("update-ref", "refs/pull/1/head", rg.head)
	sub := rg.submitAll()
	var cell18 store.Run
	for _, s := range sub.Runs {
		if s.Context == "localci/CI/test linux (linux, 18)" {
			cell18, _ = rg.st.Run(t.Context(), s.RunID)
		}
	}
	if cell18.ID == "" || len(sub.Runs) != 2 {
		t.Fatalf("submitted %+v, want the two suffixed cells", sub.Runs)
	}
	before := rg.checkRuns()
	res, err := rg.srv.retryCheck(t.Context(), cell18)
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	rg.runner.Wait()
	if got := rg.checkRuns() - before; got != 1 {
		t.Errorf("the retry started %d check runs, want 1", got)
	}
}

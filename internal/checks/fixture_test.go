package checks

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

// sh runs a command and returns its trimmed output.
func sh(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := osexec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeForge records the statuses it is given.
type fakeForge struct {
	mu       sync.Mutex
	statuses []posted
}

type posted struct {
	Repo, SHA string
	forge.Status
}

func (f *fakeForge) Watch(context.Context, func(forge.Trigger) error) error { return nil }
func (f *fakeForge) ResolveHeads(context.Context, []string, forge.Trigger) ([]forge.Head, error) {
	return nil, nil
}
func (f *fakeForge) SetStatus(_ context.Context, repo, sha string, st forge.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, posted{repo, sha, st})
	return nil
}

// of returns the statuses posted on one suite name, in order.
func (f *fakeForge) of(suite string) []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []posted
	for _, p := range f.statuses {
		if p.Suite == suite {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeForge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.statuses)
}

// fakeAdmit lets everything in at once and remembers how many ran together.
type fakeAdmit struct {
	mu       sync.Mutex
	cur, max int
}

func (a *fakeAdmit) Admit(ctx context.Context, c Cost) (func(), error) {
	a.mu.Lock()
	a.cur++
	a.max = max(a.max, a.cur)
	a.mu.Unlock()
	return func() { a.mu.Lock(); a.cur--; a.mu.Unlock() }, nil
}

// fixture is a source repository standing in for the code host, with pull
// requests as refs, and a Runner wired to it.
type fixture struct {
	t      *testing.T
	src    string
	base   string
	runner *Runner
	store  *store.Store
	forge  *fakeForge
	admit  *fakeAdmit
	data   string
}

const ciWorkflow = `name: CI
on: [pull_request]
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo ran >> "$RUNLOG"
`

func newFixture(t *testing.T, workflowYAML string) *fixture {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	fx := &fixture{t: t, src: t.TempDir(), data: t.TempDir(), forge: &fakeForge{}, admit: &fakeAdmit{}}
	sh(t, fx.src, "git", "init", "-q", "-b", "main")
	fx.write("README.md", "hello\n")
	fx.write(".github/workflows/ci.yml", workflowYAML)
	sh(t, fx.src, "git", "add", "-A")
	sh(t, fx.src, "git", "commit", "-q", "-m", "base")
	fx.base = sh(t, fx.src, "git", "rev-parse", "HEAD")

	var err error
	fx.store, err = store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fx.store.Close() })
	fx.runner = &Runner{
		Store: fx.store, Forge: fx.forge, Admit: fx.admit, Log: event.NewLog(fx.store, event.NewBus()),
		Options: Options{DataDir: fx.data, MergeRef: true, Toolchains: exec.ToolchainContainers,
			URL: func(id string) string { return "http://localhost/runs/" + id }},
		Mirror: func(ctx context.Context, repo string) (*exec.Mirror, error) {
			return exec.EnsureMirror(ctx, fx.data, repo, fx.src)
		},
	}
	t.Cleanup(fx.runner.Wait)
	t.Setenv("RUNLOG", filepath.Join(t.TempDir(), "runs.log"))
	return fx
}

func (fx *fixture) write(path, content string) {
	fx.t.Helper()
	p := filepath.Join(fx.src, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fx.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		fx.t.Fatal(err)
	}
}

// pull opens pull request n changing files; it returns the head commit.
func (fx *fixture) pull(n string, files map[string]string) string {
	fx.t.Helper()
	sh(fx.t, fx.src, "git", "checkout", "-q", "-b", "pr"+n, fx.base)
	for p, c := range files {
		fx.write(p, c)
	}
	sh(fx.t, fx.src, "git", "add", "-A")
	sh(fx.t, fx.src, "git", "commit", "-q", "-m", "change "+n)
	head := sh(fx.t, fx.src, "git", "rev-parse", "HEAD")
	sh(fx.t, fx.src, "git", "checkout", "-q", "main")
	sh(fx.t, fx.src, "git", "merge", "-q", "--no-ff", "-m", "merge "+n, "pr"+n)
	merge := sh(fx.t, fx.src, "git", "rev-parse", "HEAD")
	sh(fx.t, fx.src, "git", "reset", "-q", "--hard", fx.base)
	sh(fx.t, fx.src, "git", "update-ref", "refs/pull/"+n+"/head", head)
	sh(fx.t, fx.src, "git", "update-ref", "refs/pull/"+n+"/merge", merge)
	return head
}

// rebase gives pull request n a new head and merge commit with the same trees.
func (fx *fixture) rebase(n string) string {
	fx.t.Helper()
	head := sh(fx.t, fx.src, "git", "rev-parse", "refs/pull/"+n+"/head")
	merge := sh(fx.t, fx.src, "git", "rev-parse", "refs/pull/"+n+"/merge")
	newHead := sh(fx.t, fx.src, "git", "commit-tree", head+"^{tree}", "-p", fx.base, "-m", "rebased")
	newMerge := sh(fx.t, fx.src, "git", "commit-tree", merge+"^{tree}", "-p", fx.base, "-p", newHead, "-m", "merge rebased")
	sh(fx.t, fx.src, "git", "update-ref", "refs/pull/"+n+"/head", newHead)
	sh(fx.t, fx.src, "git", "update-ref", "refs/pull/"+n+"/merge", newMerge)
	return newHead
}

func (fx *fixture) request(pr int, head string) Request {
	return Request{Repo: "acme/web", PR: pr, Base: "main", Head: head, Trigger: "push"}
}

// start queues the checks and returns without waiting.
func (fx *fixture) start(req Request) Submission {
	fx.t.Helper()
	sub, err := fx.runner.Submit(context.Background(), req)
	if err != nil {
		fx.t.Fatalf("Submit: %v", err)
	}
	return sub
}

// submit starts the checks and waits for all of them.
func (fx *fixture) submit(req Request) Submission {
	fx.t.Helper()
	sub := fx.start(req)
	for _, r := range sub.Runs {
		fx.await(r)
	}
	return sub
}

func (fx *fixture) await(r Started) {
	fx.t.Helper()
	select {
	case <-r.Done:
	case <-time.After(60 * time.Second):
		fx.t.Fatalf("run %s did not finish", r.RunID)
	}
}

func (fx *fixture) run(id string) store.Run {
	fx.t.Helper()
	r, err := fx.store.Run(context.Background(), id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func (fx *fixture) runLog(id string) string {
	fx.t.Helper()
	b, err := os.ReadFile(filepath.Join(fx.data, "logs", "checks", id, "run.log"))
	if err != nil {
		fx.t.Fatal(err)
	}
	return string(b)
}

// executions counts the lines the workflow's steps wrote to $RUNLOG.
func executions(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("RUNLOG"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "ran\n")
}

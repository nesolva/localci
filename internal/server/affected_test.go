package server

import (
	"context"
	"encoding/json"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// pushForge resolves every trigger to one pull request head.
type pushForge struct {
	recordForge
	head forge.Head
}

func (f *pushForge) ResolveHeads(context.Context, []string, forge.Trigger) ([]forge.Head, error) {
	return []forge.Head{f.head}, nil
}

type choosesAll struct{ done chan suite.DoneInput }

func (choosesAll) Plan(_ context.Context, in suite.PlanInput) (suite.Plan, error) {
	d, _ := json.Marshal(map[string]any{"all": true, "total": len(in.Tests)})
	return suite.Plan{All: true, Detail: d}, nil
}
func (c choosesAll) Done(_ context.Context, in suite.DoneInput) { c.done <- in }

// A push on a pull request runs the affected tests and reports localci/<suite> (affected), never the suite's own
// context, which belongs to the full run that branch protection requires. The gate that holds fork heads back until
// a writer approved that exact head is the watcher's (internal/forge); what reaches Handle has passed it, and the
// run keeps the fork flag so its report teaches nothing.
func TestPushRunsAffectedAndPostsOnlyTheAffectedContext(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src, data := t.TempDir(), t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := osexec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "head")
	sha := git("rev-parse", "HEAD")
	git("update-ref", "refs/pull/1/head", sha)

	suites, err := suite.ParseYAML([]byte(`suites:
  e2e:
    repos: [acme/web]
    trigger: {push: true}
    test:
      run: |
        printf '{"v":1,"t":"begin","ts":1,"shard":1,"total":1}\n{"v":1,"t":"test.begin","ts":1,"test":"t1","file":"a.spec.ts","title":"x","line":1}\n{"v":1,"t":"test.end","ts":2,"test":"t1","status":"passed","ms":1,"retry":0}\n{"v":1,"t":"end","ts":3,"status":"passed"}\n' >> "$LOCALCI_REPORT"
      list: printf '{"test":"t1","file":"a.spec.ts","title":"x","line":1}\n'
`))
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	bus := event.NewBus()
	runner, err := suite.New(suite.Deps{Store: st, Log: event.NewLog(st, bus), Sched: sched.New(2, 0), DataDir: data, Suites: suites,
		RepoURL: func(string) string { return src }})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	aff := choosesAll{done: make(chan suite.DoneInput, 1)}
	runner.SetAffector(aff)
	fg := &pushForge{head: forge.Head{Repo: "acme/web", Branch: "feat", SHA: sha, PR: 1}}
	d := &Dispatcher{Forge: fg, Suites: runner, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id },
		Config: func() *config.Config { return &config.Config{Repos: []config.Repo{{Name: "acme/web"}}} }}

	if err := d.Handle(t.Context(), forge.Trigger{Kind: forge.KindPush, Repo: "acme/web", PR: 1, Branch: "feat", Head: sha,
		Base: "main", Fork: true, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.Runs(t.Context(), 0)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v, want one", runs)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	run, err := runner.Wait(ctx, runs[0].ID)
	if err != nil || run.State != store.StatePassed {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if !run.Affected || !run.Fork {
		t.Fatalf("run = %+v, want an affected run carrying the fork flag", run)
	}
	if got := <-aff.done; !got.Fork {
		t.Fatal("the run's report was trusted: a fork's run must not teach the map")
	}

	var last forge.Status
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got := fg.statuses(); len(got) > 0 {
			last = got[len(got)-1].st
			if last.State == forge.StateSuccess {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.State != forge.StateSuccess {
		t.Fatalf("statuses = %+v, want a final success", fg.statuses())
	}
	for _, p := range fg.statuses() {
		if forge.Context(p.st.Suite) != "localci/e2e (affected)" {
			t.Fatalf("a status was posted as %q: the full suite's context must stay free for the full run", forge.Context(p.st.Suite))
		}
	}
}

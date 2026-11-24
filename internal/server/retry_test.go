package server

import (
	"context"
	"encoding/json"
	osexec "os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// bases records the branch each affected run was asked to diff against.
type bases struct {
	mu  sync.Mutex
	got []string
}

func (b *bases) Plan(_ context.Context, in suite.PlanInput) (suite.Plan, error) {
	b.mu.Lock()
	b.got = append(b.got, in.Request.Base)
	b.mu.Unlock()
	d, _ := json.Marshal(map[string]any{"all": true, "total": len(in.Tests)})
	return suite.Plan{All: true, Detail: d}, nil
}
func (b *bases) Done(context.Context, suite.DoneInput) {}

// A retry of an affected run supersedes the run it retries (same heads), and the superseded run leaves its status
// to the run that replaces it. The retry must therefore report for itself, for the same diff, or the pull
// request's `(affected)` status stays pending for ever.
func TestRetryOfAnAffectedRunReportsItsOwnStatusAndKeepsTheBase(t *testing.T) {
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
	rec := &bases{}
	runner.SetAffector(rec)
	fg := &pushForge{head: forge.Head{Repo: "acme/web", Branch: "feat", SHA: sha, PR: 1}}
	d := &Dispatcher{Forge: fg, Suites: runner, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id },
		Config: func() *config.Config { return &config.Config{Repos: []config.Repo{{Name: "acme/web"}}} }}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	srv := New(Deps{Store: st, Bus: bus, Suites: runner,
		Report: func(run, name string, heads []forge.Head, since int64) { go d.report(ctx, run, name, heads, since) }})

	if err := d.Handle(ctx, forge.Trigger{Kind: forge.KindPush, Repo: "acme/web", PR: 1, Branch: "feat", Head: sha,
		Base: "main", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.Runs(ctx, 0)
	first, err := runner.Wait(ctx, runs[0].ID)
	if err != nil || first.State != store.StatePassed {
		t.Fatalf("first run = %+v, %v", first, err)
	}

	res, err := srv.retrySuite(ctx, first)
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	retry := res.Runs[0].ID
	if _, err := runner.Wait(ctx, retry); err != nil {
		t.Fatal(err)
	}

	rec.mu.Lock()
	got := append([]string(nil), rec.got...)
	rec.mu.Unlock()
	if len(got) != 2 || got[1] != "main" {
		t.Fatalf("branches the runs diffed against = %v, want the retry to keep main", got)
	}
	var last forge.Status
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && last.State != forge.StateSuccess {
		for _, p := range fg.statuses() {
			if p.st.TargetURL == "http://x/runs/"+retry {
				last = p.st
				if forge.Context(p.st.Suite) != "localci/e2e (affected)" {
					t.Fatalf("the retry posted as %q", forge.Context(p.st.Suite))
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.State != forge.StateSuccess {
		t.Fatalf("the retry's last status = %+v, want success: nothing reported for it", last)
	}
}

package server

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
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

// A retry of a full pull request run replaces a label run that is still queued. The replaced run leaves its
// status to the run that replaces it, so the retry must report for itself or `localci/<suite>` stays pending.
func TestRetrySupersedesLabelRunStatusPosted(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src, data := t.TempDir(), t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")
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
	git("update-ref", "refs/pull/2/head", sha)

	// Every run of the suite waits for the gate file before it reports a passing test.
	suites, err := suite.ParseYAML([]byte(`suites:
  e2e:
    repos: [acme/web]
    trigger: {label: run-e2e}
    test:
      run: |
        while [ ! -f '` + gate + `' ]; do sleep 0.05; done
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
	fg := &pushForge{head: forge.Head{Repo: "acme/web", Branch: "feat", SHA: sha, PR: 1}}
	d := &Dispatcher{Forge: fg, Suites: runner, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id },
		Config: func() *config.Config { return &config.Config{Repos: []config.Repo{{Name: "acme/web"}}} }}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	srv := New(Deps{Store: st, Bus: bus, Suites: runner,
		Report: func(run, name string, heads []forge.Head, since int64) { go d.report(ctx, run, name, heads, since) }})
	label := forge.Trigger{Kind: forge.KindLabel, Label: "run-e2e", Repo: "acme/web", PR: 1, Branch: "feat", Head: sha, Base: "main", Attempt: 1}

	// A finished full run: the one to retry.
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Handle(ctx, label); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.Runs(ctx, 0)
	first, err := runner.Wait(ctx, runs[0].ID)
	if err != nil || first.State != store.StatePassed {
		t.Fatalf("first run = %+v, %v", first, err)
	}

	// Another pull request's run holds the suite, so the next label run for pull request 1 waits in the queue.
	os.Remove(gate)
	if _, err := runner.Submit(suite.Request{Suite: "e2e", Trigger: "label",
		Heads: []suite.Head{{Head: forge.Head{Repo: "acme/web", Branch: "other", SHA: sha, PR: 2}}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.Handle(ctx, label); err != nil {
		t.Fatal(err)
	}
	res, err := srv.retrySuite(ctx, first)
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	retry := res.Runs[0].ID
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Wait(ctx, retry); err != nil {
		t.Fatal(err)
	}

	var last forge.Status
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && last.State != forge.StateSuccess {
		last = forge.Status{}
		for _, p := range fg.statuses() {
			if p.st.Suite == "e2e" && p.sha == sha {
				last = p.st
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.State != forge.StateSuccess || last.TargetURL != "http://x/runs/"+retry {
		t.Fatalf("last status of localci/e2e = %+v, want success from the retry %s: it is pending for ever", last, retry)
	}
}

package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if origin != "" {
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", origin).CombinedOutput(); err != nil {
			t.Fatalf("remote add: %v: %s", err, out)
		}
	}
	return dir
}

func TestCheckoutNamesTheRepoWhateverTheRemoteHostIs(t *testing.T) {
	for _, url := range []string{
		"https://github.com/acme/web.git",
		"git@github.com:acme/web.git",
		"git@github-work:acme/web.git", // an SSH host alias
		"ssh://git@gh.internal.example/acme/web",
		"https://github.com/acme/web/",
	} {
		t.Run(url, func(t *testing.T) {
			co, err := checkoutOf(context.Background(), gitRepo(t, url), nil)
			if err != nil || co.repo != "acme/web" {
				t.Fatalf("repo = %q, err = %v; want acme/web", co.repo, err)
			}
		})
	}
}

func TestCheckoutReadsTheConfiguredURLNotTheRewrittenOne(t *testing.T) {
	dir := gitRepo(t, "gh:acme/web.git")
	if out, err := exec.Command("git", "-C", dir, "config", "url.https://example.invalid/mirror/.insteadOf", "gh:").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	co, err := checkoutOf(context.Background(), dir, nil)
	if err != nil || co.repo != "acme/web" {
		t.Fatalf("repo = %q, err = %v; want acme/web", co.repo, err)
	}
}

func TestSuiteHeadsRefuseADirectoryOutsideTheSuite(t *testing.T) {
	sd := &suite.Suite{Name: "e2e", Repos: []string{"acme/web", "acme/api"}}
	h, err := suiteHeads(context.Background(), sd, gitRepo(t, "git@github-work:acme/web.git"), nil)
	if err != nil || len(h) != 1 || h[0].Repo != "acme/web" || h[0].Dir == "" {
		t.Fatalf("heads = %+v, err = %v", h, err)
	}
	_, err = suiteHeads(context.Background(), sd, gitRepo(t, "git@github.com:acme/other.git"), nil)
	if err == nil || !strings.Contains(err.Error(), "acme/other") || !strings.Contains(err.Error(), "acme/web") {
		t.Fatalf("err = %v; want one naming the repo and the suite's repos", err)
	}
	if _, err = suiteHeads(context.Background(), sd, t.TempDir(), nil); err == nil {
		t.Fatal("a directory that is not a git checkout must be refused")
	}
}

func TestCheckoutMappedByPathWinsOverOrigin(t *testing.T) {
	ctx := context.Background()
	noRemote := gitRepo(t, "")
	wrongOrigin := gitRepo(t, "git@github.com:acme/fork-of-something.git")
	repos := []config.Repo{{Name: "acme/local", Path: realPath(t, noRemote)}, {Name: "acme/mapped", Path: realPath(t, wrongOrigin)}}

	for dir, want := range map[string]string{noRemote: "acme/local", wrongOrigin: "acme/mapped"} {
		co, err := checkoutOf(ctx, dir, repos)
		if err != nil || co.repo != want || co.sha == "" {
			t.Errorf("checkoutOf(%s) = %+v, %v; want repo %s", dir, co, err, want)
		}
	}
	// From a folder inside the mapped checkout, the top level is what is compared.
	sub := filepath.Join(noRemote, "pkg")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if co, err := checkoutOf(ctx, sub, repos); err != nil || co.repo != "acme/local" {
		t.Errorf("from a subfolder: %+v, %v", co, err)
	}
	// Another checkout is not the mapped one, and the error says how to map it.
	_, err := checkoutOf(ctx, gitRepo(t, ""), repos)
	if err == nil || !strings.Contains(err.Error(), "no `origin` remote") || !strings.Contains(err.Error(), "`path`") {
		t.Fatalf("err = %v; want the missing-origin error with the hint to map it with `path`", err)
	}
}

// A checkout with no remote, mapped by `path`, submits a suite run for the mapped repository.
func TestSuiteRunFromACheckoutWithoutOriginIsTheMappedRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := gitRepo(t, "")
	suites, err := suite.ParseYAML([]byte(`suites:
  e2e:
    repos: [acme/local]
    test:
      run: |
        printf '{"v":1,"t":"begin","ts":1,"shard":1,"total":1}\n{"v":1,"t":"test.begin","ts":1,"test":"t1","file":"a.spec.ts","title":"x","line":1}\n{"v":1,"t":"test.end","ts":2,"test":"t1","status":"passed","ms":1,"retry":0}\n{"v":1,"t":"end","ts":3,"status":"passed"}\n' >> "$LOCALCI_REPORT"
      list: printf '{"test":"t1","file":"a.spec.ts","title":"x","line":1}\n'
`))
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	mapped := &config.Effective{Config: config.Config{Repos: []config.Repo{{Name: "acme/local", Path: realPath(t, dir)}}}}
	runner, err := suite.New(suite.Deps{Store: st, Log: event.NewLog(st, event.NewBus()), Sched: sched.New(2, 0), DataDir: t.TempDir(),
		Suites: suites, RepoURL: localRepoURL(mapped.Repos)})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	srv := New(Deps{Store: st, Bus: event.NewBus(), Suites: runner, Config: func() (*config.Effective, error) { return mapped, nil }})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	res, err := srv.submitSuite(ctx, SubmitRequest{Suite: "e2e", Dir: dir})
	if err != nil || len(res.Runs) != 1 {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	if h := res.Runs[0].Heads; len(h) != 1 || h[0].Repo != "acme/local" {
		t.Fatalf("heads = %+v, want one for acme/local", h)
	}
	if run, err := runner.Wait(ctx, res.Runs[0].ID); err != nil || run.State != store.StatePassed {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// The same checkout, unmapped, is refused with the hint.
	srv = New(Deps{Store: st, Bus: event.NewBus(), Suites: runner})
	if _, err = srv.submitSuite(ctx, SubmitRequest{Suite: "e2e", Dir: dir}); err == nil || !strings.Contains(err.Error(), "`path`") {
		t.Fatalf("err = %v; want the hint to map the checkout with `path`", err)
	}
}

func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

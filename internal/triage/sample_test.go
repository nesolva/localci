package triage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
	"github.com/nesolva/localci/internal/triage"
)

// The sample suite is the real thing: the Playwright package and the example shop app, driven through the
// runner. It needs `npm install` at the repository root and `npm run build` in packages/playwright. Each run
// takes about a minute and a half.

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	root := filepath.Join(wd, "..", "..")
	for _, need := range []string{"node_modules/.bin/localci-playwright", "packages/playwright/dist/cli.js", "examples/acme-shop/package.json"} {
		if _, err := os.Stat(filepath.Join(root, need)); err != nil {
			t.Skipf("sample suite needs %s (npm install; npm run build -w packages/playwright): %v", need, err)
		}
	}
	if _, err := osexec.LookPath("node"); err != nil {
		t.Skip("sample suite needs Node")
	}
	return root
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

type sampleEnv struct {
	r    *suite.Runner
	st   *store.Store
	src  string // the shop's repository: what the runs test, and where a patch applies
	data string
	t    *testing.T
}

// newSample copies the example shop into a git repository and sets a runner up for it with 2 shards, the given
// test environment and the suite's `triage` key.
func newSample(t *testing.T, testEnv map[string]string, triageYAML string) *sampleEnv {
	t.Helper()
	root := repoRoot(t)
	src, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(src, os.DirFS(filepath.Join(root, "examples", "acme-shop"))); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(src, "node_modules"))
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "sample")

	g := &sampleEnv{t: t, src: src, data: t.TempDir()}
	settings := filepath.Join(t.TempDir(), "global-settings.json")
	env := map[string]string{"PORT": `{{port "web"}}`, "DB_PATH": "{{db.url}}", "ACME_GLOBAL_SETTINGS": settings}
	for k, v := range testEnv {
		env[k] = v
	}
	envYAML := ""
	for k, v := range env {
		b, _ := json.Marshal(v)
		envYAML += fmt.Sprintf("        %s: %s\n", k, b)
	}
	yaml := fmt.Sprintf(`suites:
  shop:
    repos: [acme/shop]
    prepare:
      - repo: acme/shop
        run: 'ln -sfn %s/node_modules node_modules'
        cache_key: [package.json]
        keep: [node_modules]
    database:
      driver: command
      template: {seed: "echo '{\"tenants\":{}}' > {{db.url}}"}
      create: "cp {{template}} {{name}}"
      drop: "rm -f {{name}}"
    test:
      repo: acme/shop
      run: npx playwright test
      env:
%s    shards: 2
    timeout: 10m
%s`, root, envYAML, triageYAML)
	suites, err := suite.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if g.st, err = store.Open(filepath.Join(g.data, "store.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.st.Close() })
	// The runner needs the address its hooks are served at before it exists: the server forwards to it later.
	var hooks http.Handler
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hooks.ServeHTTP(w, r) }))
	g.r, err = suite.New(suite.Deps{Store: g.st, Log: event.NewLog(g.st, event.NewBus()), Sched: sched.New(3, 0),
		DataDir: g.data, Suites: suites, RepoURL: func(string) string { return src }, API: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	hooks = g.r.Handler()
	t.Cleanup(func() { g.r.Close(); api.Close() })
	return g
}

func (g *sampleEnv) run() store.Run {
	g.t.Helper()
	id, err := g.r.Submit(suite.Request{Suite: "shop", Heads: []suite.Head{{Head: forge.Head{Repo: "acme/shop", Branch: "main"}}}})
	if err != nil {
		g.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run, err := g.r.Wait(ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return run
}

// tests is the last attempt of every test of the run.
func (g *sampleEnv) tests(run string) []store.Test {
	g.t.Helper()
	ts, err := g.st.RunTests(context.Background(), run)
	if err != nil {
		g.t.Fatal(err)
	}
	return ts
}

func (g *sampleEnv) byClass(run, class string) []store.Test {
	var out []store.Test
	for _, tc := range g.tests(run) {
		if tc.Class == class {
			out = append(out, tc)
		}
	}
	return out
}

func (g *sampleEnv) dump(run store.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s (%s)\n", run.State, run.Reason)
	for _, tc := range g.tests(run.ID) {
		if tc.Class != "" || tc.Status != "passed" {
			fmt.Fprintf(&b, "  %s %s class=%q evidence=%s\n", tc.Title, tc.Status, tc.Class, tc.Evidence)
		}
	}
	return b.String()
}

func TestSampleTriageRenamedButtonIsATestBugWhosePatchMakesTheTestPass(t *testing.T) {
	g := newSample(t, map[string]string{"ACME_RENAME": "1"}, "")
	run := g.run()
	if run.State != store.StateFailed {
		t.Fatalf("%s", g.dump(run))
	}
	bugs := g.byClass(run.ID, triage.TestBug)
	if len(bugs) == 0 {
		t.Fatalf("no test bug:\n%s", g.dump(run))
	}
	var target store.Test
	var info *triage.PatchInfo
	for _, tc := range bugs {
		if ev := triage.ReadEvidence(tc.Evidence); ev.Patch != nil && tc.Title == "paying creates an order" {
			target, info = tc, ev.Patch
		}
		if n := len(triage.ReadEvidence(tc.Evidence).Lines); n > 3 {
			t.Errorf("%s: %d evidence lines", tc.Title, n)
		}
	}
	if info == nil {
		t.Fatalf("the paying test has no patch:\n%s", g.dump(run))
	}
	if info.From != "Pay" || info.To != "Place order" || info.File != "tests/checkout.spec.ts" {
		t.Fatalf("patch info = %+v", info)
	}
	if err := triage.Apply(context.Background(), g.src, *info, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := git(t, g.src, "diff", "--stat"); !strings.Contains(got, "tests/checkout.spec.ts") || strings.Count(got, "\n") != 1 {
		t.Fatalf("the patch changed more than the one file:\n%s", got)
	}

	// The same environment, the tree with the patch: the test passes. The run tests the branch, so commit it.
	git(t, g.src, "commit", "-qam", "fix the selector")
	again := g.run()
	for _, tc := range g.tests(again.ID) {
		if tc.ID == target.ID && tc.Status != "passed" {
			t.Fatalf("%s is still %s after the patch:\n%s", tc.Title, tc.Status, g.dump(again))
		}
	}
}

func TestSampleTriageServerErrorIsAProductBug(t *testing.T) {
	g := newSample(t, map[string]string{"ACME_500": "1"}, "")
	run := g.run()
	if run.State != store.StateFailed {
		t.Fatalf("%s", g.dump(run))
	}
	if bugs := g.byClass(run.ID, triage.ProductBug); len(bugs) == 0 {
		t.Fatalf("no product bug:\n%s", g.dump(run))
	} else if ev := triage.ReadEvidence(bugs[0].Evidence); !strings.Contains(strings.Join(ev.Lines, "\n"), "500") {
		t.Fatalf("evidence has no 500: %v", ev.Lines)
	}
}

func TestSampleTriageFlakyAddToCartIsFlaky(t *testing.T) {
	// Add-to-cart skips silently half of the time: a test that fails and then passes when it is run alone again
	// is flaky. Up to four runs make sure a skip happens.
	var last store.Run
	var g *sampleEnv
	for i := 0; i < 4; i++ {
		g = newSample(t, map[string]string{"ACME_FLAKY": "1"}, "    triage: {reruns: 3}\n")
		last = g.run()
		if flaky := g.byClass(last.ID, triage.Flaky); len(flaky) > 0 {
			ev := triage.ReadEvidence(flaky[0].Evidence)
			if len(ev.Lines) == 0 || len(ev.Lines) > 3 {
				t.Fatalf("flaky evidence: %v", ev.Lines)
			}
			if last.State == store.StatePassed && ev.Note == "" {
				t.Fatalf("a passed run says nothing about its flaky test: %s", g.dump(last))
			}
			return
		}
		t.Logf("run %d had no skipped add-to-cart: %s", i+1, g.dump(last))
	}
	t.Fatalf("four runs of a 50%% flaky suite classed nothing flaky:\n%s", g.dump(last))
}

func TestSampleTriageStackKilledMidRunIsInfrastructure(t *testing.T) {
	g := newSample(t, nil, "")
	var once sync.Once
	g.r.OnTest(func(c suite.TestCtx) {
		if c.Shard == 0 || c.Ports["web"] == 0 {
			return
		}
		// The first test of a shard loses its stack: whatever listens on the shard's web port is killed.
		once.Do(func() {
			out, _ := osexec.Command("lsof", "-ti", "tcp:"+strconv.Itoa(c.Ports["web"])).Output()
			for _, pid := range strings.Fields(string(out)) {
				osexec.Command("kill", "-9", pid).Run()
			}
			time.Sleep(500 * time.Millisecond)
		})
	}, nil)
	run := g.run()
	if infra := g.byClass(run.ID, triage.Infrastructure); len(infra) == 0 {
		t.Fatalf("a killed stack is not infrastructure:\n%s", g.dump(run))
	}
}

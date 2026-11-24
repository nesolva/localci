package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
)

// The sample suite is the real thing: the Playwright package and the example shop app, driven through the
// runner. It needs `npm install` at the repository root and `npm run build` in packages/playwright.

// repoRoot finds the repository root from the test's directory.
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

// sampleSlowMS is how long every page of the sample app waits, so that test time dominates the fixed costs of
// starting a shard.
const sampleSlowMS = "1500"

type sampleEnv struct {
	r        *Runner
	st       *store.Store
	data     string
	t        *testing.T
	api      *httptest.Server
	gate     *gate
	settings string
}

// newSample copies the example shop into a git repository and sets a runner up for it with the given shard
// count and extra test environment (KEY: value lines under test.env).
func newSample(t *testing.T, shards int, testEnv map[string]string) *sampleEnv {
	t.Helper()
	return newSampleWith(t, shards, testEnv, "")
}

// newSampleWith is newSample with extra suite lines (indented four spaces) added to the suite.
func newSampleWith(t *testing.T, shards int, testEnv map[string]string, extra string) *sampleEnv {
	t.Helper()
	root := repoRoot(t)
	src := t.TempDir()
	if err := os.CopyFS(src, os.DirFS(filepath.Join(root, "examples", "acme-shop"))); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(src, "node_modules"))
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "sample")

	g := &sampleEnv{t: t, data: t.TempDir(), settings: filepath.Join(t.TempDir(), "global-settings.json")}
	env := map[string]string{"PORT": `{{port "web"}}`, "DB_PATH": "{{db.url}}", "ACME_GLOBAL_SETTINGS": g.settings}
	for k, v := range testEnv {
		env[k] = v
	}
	extra = strings.ReplaceAll(extra, "@SETTINGS@", g.settings) // the global settings file the stack must share with the tests
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
%s    shards: %d
    timeout: 10m
%s`, root, envYAML, shards, extra)
	suites, err := ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if g.st, err = store.Open(filepath.Join(g.data, "store.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.st.Close() })
	g.r, err = New(Deps{Store: g.st, Log: event.NewLog(g.st, event.NewBus()), Sched: sched.New(max(shards, 1)+1, 0),
		DataDir: g.data, Suites: suites, RepoURL: func(string) string { return src }})
	if err != nil {
		t.Fatal(err)
	}
	g.gate = hookGate(g.r)
	g.api = httptest.NewServer(g.gate)
	g.r.d.API = g.api.URL
	t.Cleanup(func() { g.r.Close(); g.api.Close() })
	return g
}

func (g *sampleEnv) run() (store.Run, time.Duration) {
	g.t.Helper()
	began := time.Now()
	id, err := g.r.Submit(Request{Suite: "shop", Heads: []Head{{Head: forge.Head{Repo: "acme/shop", Branch: "main"}}}})
	if err != nil {
		g.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run, err := g.r.Wait(ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	// The fixture's hook calls fail silently, so check here that they carry the run's token.
	if g.gate.passed.Load() == 0 || g.gate.refused.Load() != 0 {
		g.t.Errorf("hook calls: %d accepted, %d refused for their token; the fixture must send $LOCALCI_HOOK_TOKEN",
			g.gate.passed.Load(), g.gate.refused.Load())
	}
	return run, time.Since(began)
}

func (g *sampleEnv) planned(run string) (tests int) {
	g.t.Helper()
	evs, err := g.st.RunEvents(context.Background(), run)
	if err != nil {
		g.t.Fatal(err)
	}
	for _, e := range evs {
		if e.Kind == "run.planned" {
			var p struct{ Tests int }
			json.Unmarshal(e.Data, &p)
			tests = p.Tests
		}
	}
	return tests
}

func TestSampleSuiteRunsEveryTestOnItsOwnDatabaseAndPort(t *testing.T) {
	g := newSample(t, 3, nil)
	var mu sync.Mutex
	dbOf, portOf := map[int]string{}, map[int]int{}
	g.r.OnTest(func(c TestCtx) {
		mu.Lock()
		defer mu.Unlock()
		if c.Shard > 0 {
			dbOf[c.Shard], portOf[c.Shard] = c.DB, c.Ports["web"]
		}
	}, nil)

	run, _ := g.run()
	if run.State != store.StatePassed {
		t.Fatalf("run %s (%s)", run.State, run.Reason)
	}
	total := g.planned(run.ID)
	if total < 6 {
		t.Fatalf("the sample suite planned only %d tests", total)
	}
	got := last(readAttempts(t, g.data, run.ID))
	if len(got) != total {
		t.Errorf("stored %d tests of %d", len(got), total)
	}
	for id, a := range got {
		if a.Status != "passed" {
			t.Errorf("%s: %s", id, a.Status)
		}
	}

	mu.Lock()
	dbs, ports := map[string]bool{}, map[int]bool{}
	for n := range dbOf {
		dbs[dbOf[n]], ports[portOf[n]] = true, true
	}
	if len(dbOf) != 3 || len(dbs) != 3 || len(ports) != 3 || ports[0] {
		t.Errorf("each of 3 shards needs its own database and port: dbs %v ports %v", dbOf, portOf)
	}
	mu.Unlock()

	lanes, err := g.st.Timeline(context.Background(), run.ID)
	if err != nil || len(lanes) < 3 {
		t.Fatalf("timeline lanes = %d, %v", len(lanes), err)
	}
	for _, l := range lanes {
		if len(l.Steps) == 0 {
			t.Errorf("lane %d has no steps", l.Shard)
		}
	}
	var left []string
	filepath.WalkDir(filepath.Join(g.data, "state", "shop", "db"), func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() && !strings.HasPrefix(d.Name(), "localci_tpl_") {
			left = append(left, d.Name())
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("shard databases left behind: %v", left)
	}
}

func TestSampleSuiteBrokenAppStoresTheFailureEvidence(t *testing.T) {
	g := newSample(t, 2, map[string]string{"ACME_BREAK": "1"})
	run, _ := g.run()
	if run.State != store.StateFailed {
		t.Fatalf("run %s (%s), want failed", run.State, run.Reason)
	}
	fails, err := g.st.Failures(context.Background(), run.ID)
	if err != nil || len(fails) == 0 {
		t.Fatalf("no failures stored: %v", err)
	}
	var f struct {
		Message string
		Aria    string
		Console []string
		Network []struct{ URL string }
	}
	for _, tc := range fails {
		if err := json.Unmarshal(tc.Failure, &f); err != nil {
			t.Fatalf("%s: %v", tc.ID, err)
		}
		if f.Message == "" || f.Aria == "" {
			t.Errorf("%s: failure report lacks message or aria: %s", tc.ID, tc.Failure)
		}
	}
	// The broken total shows up in at least one failure with the console and network evidence of its page.
	if !strings.Contains(string(fails[0].Failure), `"console"`) || !strings.Contains(string(fails[0].Failure), `"network"`) {
		t.Errorf("failure lacks console and network fields: %s", fails[0].Failure)
	}
}

func TestSampleSuiteThreeShardsAreFasterThanOne(t *testing.T) {
	if testing.Short() {
		t.Skip("times two full runs")
	}
	slow := map[string]string{"SLOW_MS": sampleSlowMS}
	one, three := newSample(t, 1, slow), newSample(t, 3, slow)
	// Each suite runs twice and the second run is timed: the first one learns the durations and the conflicts
	// of the suite, so it is not what a team sees day to day.
	var d1, d3 time.Duration
	for i := 0; i < 2; i++ {
		var r1, r3 store.Run
		r1, d1 = one.run()
		r3, d3 = three.run()
		for _, c := range []struct {
			env *sampleEnv
			run store.Run
		}{{one, r1}, {three, r3}} {
			if c.run.State != store.StatePassed {
				fails, _ := c.env.st.Failures(context.Background(), c.run.ID)
				for _, f := range fails {
					t.Logf("failed: %s %s", f.ID, f.Failure)
				}
				t.Fatalf("run %s (%s)", c.run.State, c.run.Reason)
			}
		}
	}
	t.Logf("SLOW_MS="+sampleSlowMS+", second run of each: 1 shard %s, 3 shards %s", d1.Round(time.Millisecond), d3.Round(time.Millisecond))
	// Wall-clock ratios depend on what else the machine is doing (go test runs packages side by side), so the
	// default only guards that sharding helps; LOCALCI_STRICT_TIMING=1 on an idle machine asks for the 2x of the
	// task's criterion.
	if d3 >= d1 {
		t.Errorf("3 shards (%s) should be faster than 1 shard (%s)", d3, d1)
	}
	if os.Getenv("LOCALCI_STRICT_TIMING") == "1" && d3*2 > d1 {
		t.Errorf("3 shards (%s) should take at most half of 1 shard (%s)", d3, d1)
	}
}

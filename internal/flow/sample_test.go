package flow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/flow"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// The sample is the real thing: the Playwright package and the example shop, driven through the runner with the
// flow hooks attached. It needs `npm install` at the repository root and `npm run build -w packages/playwright`.
func TestSampleSecondTestRestoresLoginAndIsFaster(t *testing.T) {
	wd, _ := os.Getwd()
	root := filepath.Join(wd, "..", "..")
	for _, need := range []string{"node_modules/.bin/localci-playwright", "packages/playwright/dist/cli.js", "examples/acme-shop/flows/login.ts"} {
		if _, err := os.Stat(filepath.Join(root, need)); err != nil {
			t.Skipf("sample needs %s (npm install; npm run build -w packages/playwright): %v", need, err)
		}
	}
	if _, err := osexec.LookPath("node"); err != nil {
		t.Skip("sample needs Node")
	}
	src := t.TempDir()
	if err := os.CopyFS(src, os.DirFS(filepath.Join(root, "examples", "acme-shop"))); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(src, "node_modules"))
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "sample")

	data := t.TempDir()
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
      snapshot: "cp {{template}} {{name}}"
    test:
      repo: acme/shop
      run: npx playwright test
      env:
        PORT: "{{port \"web\"}}"
        DB_PATH: "{{db.url}}"
        SLOW_MS: "1500"
    shards: 1
    timeout: 10m
`, root)
	suites, err := suite.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var handler http.Handler
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(api.Close)
	r, err := suite.New(suite.Deps{Store: st, Log: event.NewLog(st, event.NewBus()), Sched: sched.New(2, 0), DataDir: data,
		Suites: suites, RepoURL: func(string) string { return src }, API: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	handler = r.Handler()
	flow.Attach(r, &flow.Service{Store: st, DataDir: data})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var all strings.Builder
	runOnce := func() []flowCall {
		t.Helper()
		id, err := r.Submit(suite.Request{Suite: "shop", Tests: suite.Filter{Files: []string{"flows.spec.ts"}},
			Heads: []suite.Head{{Head: forge.Head{Repo: "acme/shop", Branch: "main"}}}})
		if err != nil {
			t.Fatal(err)
		}
		run, err := r.Wait(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if run.State != store.StatePassed {
			t.Fatalf("run %s (%s)", run.State, run.Reason)
		}
		evs, err := st.RunEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return callsOf(evs, &all)
	}
	first := runOnce()
	// Two tests, each creating the shop and logging in: the first saves, the second restores.
	expect(t, "first run", first, "create-shop saved", "login saved", "create-shop restored", "login restored")
	calls := first
	if calls[3].ms >= calls[1].ms {
		t.Errorf("restoring login took %.0f ms, running it %.0f ms: the checkpoint did not make it faster", calls[3].ms, calls[1].ms)
	}
	t.Logf("login: run %.0f ms, restore %.0f ms; create-shop: run %.0f ms, restore %.0f ms", calls[1].ms, calls[3].ms, calls[0].ms, calls[2].ms)

	// The same commit again reuses every checkpoint, across runs.
	expect(t, "same commit", runOnce(), "create-shop restored", "login restored", "create-shop restored", "login restored")
	// A new commit gives new keys: nothing is restored from the old one.
	if err := os.WriteFile(filepath.Join(src, "NOTES.md"), []byte("a new commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "second")
	expect(t, "new commit", runOnce(), "create-shop saved", "login saved", "create-shop restored", "login restored")

	if strings.Contains(all.String(), "demo123") || strings.Contains(all.String(), `"cookies"`) || strings.Contains(all.String(), "sid") {
		t.Errorf("a password or storage state reached the events")
	}
	if !strings.Contains(all.String(), `"password":"***"`) {
		t.Errorf("the password input does not show as ***")
	}
	ents, _ := os.ReadDir(filepath.Join(data, "checkpoints", "shop"))
	if len(ents) != 4 {
		t.Errorf("checkpoint files = %d, want 4 (two flows for each of two commits)", len(ents))
	}
}

type flowCall struct {
	flow, checkpoint string
	ms               float64
}

// callsOf lists the flow calls of a run in order, and appends every event body to all.
func callsOf(evs []store.Event, all *strings.Builder) []flowCall {
	var calls []flowCall
	flowOf := map[string]string{}
	for _, e := range evs {
		all.Write(e.Data)
		switch e.Kind {
		case "flow.begin":
			var b struct{ Flow string }
			json.Unmarshal(e.Data, &b)
			flowOf[e.Step] = b.Flow
		case "flow.end":
			var b struct {
				Checkpoint string
				MS         float64
			}
			json.Unmarshal(e.Data, &b)
			calls = append(calls, flowCall{flowOf[e.Step], b.Checkpoint, b.MS})
		}
	}
	return calls
}

func expect(t *testing.T, what string, got []flowCall, want ...string) {
	t.Helper()
	var have []string
	for _, c := range got {
		have = append(have, c.flow+" "+c.checkpoint)
	}
	if strings.Join(have, ", ") != strings.Join(want, ", ") {
		t.Errorf("%s: flow calls = %v, want %v", what, have, want)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: %d calls, want %d", what, len(got), len(want))
	}
}

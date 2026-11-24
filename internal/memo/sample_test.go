package memo_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
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

// sample is the real thing: the Playwright package and the example shop, with twenty tests that share
// create-shop and login and five create-order variants, driven through the runner with the flow hooks attached.
// It needs `npm install` at the repository root and `npm run build -w packages/playwright`.
type sample struct {
	t      *testing.T
	root   string
	src    string
	marker string
	rigs   map[string]*rig
}

type rig struct {
	r    *suite.Runner
	st   *store.Store
	data string
}

func newSample(t *testing.T) *sample {
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
	s := &sample{t: t, root: root, src: t.TempDir(), rigs: map[string]*rig{}}
	s.marker = filepath.Join(t.TempDir(), "refuse-once")
	if err := os.CopyFS(s.src, os.DirFS(filepath.Join(root, "examples", "acme-shop"))); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(s.src, "node_modules"))
	for from, to := range map[string]string{
		"create-order.ts": "flows/create-order.ts", "session.ts": "flows/session.ts",
		"memo.spec.ts": "tests/memo.spec.ts", "memo-verify.spec.ts": "tests/memo-verify.spec.ts"} {
		b, err := os.ReadFile(filepath.Join("testdata", from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.src, to), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, s.src, "init", "-q", "-b", "main")
	git(t, s.src, "add", "-A")
	git(t, s.src, "commit", "-q", "-m", "sample")
	return s
}

// rig makes a runner of its own (its own data directory, so its checkpoints and learned prefixes are its own);
// extra is added under the suite.
func (s *sample) rig(name, extra string) *rig {
	t := s.t
	if g := s.rigs[name]; g != nil {
		return g
	}
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
        SLOW_MS: "1000"
        VERIFY_FAIL_MARKER: %s
    shards: 4
    timeout: 15m
%s`, s.root, s.marker, extra)
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
	r, err := suite.New(suite.Deps{Store: st, Log: event.NewLog(st, event.NewBus()), Sched: sched.New(4, 0), DataDir: data,
		Suites: suites, RepoURL: func(string) string { return s.src }, API: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	handler = r.Handler()
	flow.Attach(r, &flow.Service{Store: st, DataDir: data})
	g := &rig{r: r, st: st, data: data}
	s.rigs[name] = g
	return g
}

// commit makes a new commit, so every flow key is new: the next run starts with no checkpoints.
func (s *sample) commit(n int) {
	if err := os.WriteFile(filepath.Join(s.src, "NOTES.md"), []byte(fmt.Sprintf("commit %d\n", n)), 0o644); err != nil {
		s.t.Fatal(err)
	}
	git(s.t, s.src, "add", "-A")
	git(s.t, s.src, "commit", "-q", "-m", fmt.Sprintf("c%d", n))
}

type outcome struct {
	run     store.Run
	wall    time.Duration
	tests   map[string]string
	saved   []string // flows that ran, as "<flow> <inputs>", from the events
	started int      // flow calls in all
}

func (s *sample) run(g *rig, file string) outcome {
	t := s.t
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	began := time.Now()
	id, err := g.r.Submit(suite.Request{Suite: "shop", Tests: suite.Filter{Files: []string{file}},
		Heads: []suite.Head{{Head: forge.Head{Repo: "acme/shop", Branch: "main"}}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := g.r.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if fs, _ := g.st.Failures(ctx, id); len(fs) > 0 {
		for _, f := range fs {
			t.Logf("failed: %s: %.300s", f.Title, f.Failure)
		}
	}
	o := outcome{run: run, wall: time.Since(began), tests: map[string]string{}}
	ts, _ := g.st.RunTests(ctx, id)
	for _, tc := range ts {
		o.tests[tc.ID] = tc.Status
	}
	evs, err := g.st.RunEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	what := map[string]string{}
	for _, e := range evs {
		switch e.Kind {
		case "flow.begin":
			var b struct {
				Flow   string
				Inputs map[string]any
			}
			json.Unmarshal(e.Data, &b)
			in, _ := json.Marshal(b.Inputs)
			what[e.Step] = b.Flow + " " + string(in)
		case "flow.end":
			var b struct{ Checkpoint string }
			json.Unmarshal(e.Data, &b)
			o.started++
			if b.Checkpoint != "restored" {
				o.saved = append(o.saved, what[e.Step])
			}
		}
	}
	return o
}

func (o outcome) mustPass(t *testing.T, want int) {
	t.Helper()
	if o.run.State != store.StatePassed {
		t.Fatalf("run %s (%s)", o.run.State, o.run.Reason)
	}
	for id, st := range o.tests {
		if st != "passed" {
			t.Errorf("%s: %s", id, st)
		}
	}
	if len(o.tests) != want {
		t.Fatalf("%d tests ran, want %d", len(o.tests), want)
	}
}

func distinct(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Twenty tests share create-shop and login, and five variants of create-order below them: every flow runs once
// per distinct prefix, the pass/fail set is the unmemoized run's, and the memoized run is faster.
func TestSampleSharedPrefixesRunOnceAndSaveTime(t *testing.T) {
	s := newSample(t)
	plain, memoized := s.rig("plain", "    memo: false\n"), s.rig("memo", "")

	const pairs = 3
	var plainWall, memoWall []time.Duration
	for i := 1; i <= pairs; i++ {
		s.commit(i) // new keys for both: a cold start each
		order := []*rig{plain, memoized}
		if i%2 == 0 {
			order = []*rig{memoized, plain}
		}
		for _, g := range order {
			o := s.run(g, "memo.spec.ts")
			o.mustPass(t, 20)
			if g == plain {
				if i > 1 {
					plainWall = append(plainWall, o.wall)
				}
				continue
			}
			if i == 1 {
				continue // learns the prefixes; runs like a plain one
			}
			memoWall = append(memoWall, o.wall)
			// create-shop and login once, and one create-order for each of the five variants.
			if got := len(distinct(o.saved)); len(o.saved) != 7 || got != 7 {
				t.Errorf("pair %d: %d flows ran (%d distinct), want the 7 distinct prefixes of 60 flow calls (%d seen): %v", i, len(o.saved), got, o.started, o.saved)
			}
		}
	}
	var p, m time.Duration
	for i := range plainWall {
		p += plainWall[i]
		m += memoWall[i]
	}
	p, m = p/time.Duration(len(plainWall)), m/time.Duration(len(memoWall))
	t.Logf("wall time, mean of %d cold runs: memo off %.1f s, memo on %.1f s (%.0f %% lower)", len(plainWall), p.Seconds(), m.Seconds(), 100*(1-m.Seconds()/p.Seconds()))
	if m >= p {
		t.Errorf("memoized %s is not faster than plain %s", m, p)
	}
	if os.Getenv("LOCALCI_STRICT_TIMING") == "1" && m.Seconds() > 0.7*p.Seconds() {
		t.Errorf("memoized %s is not 30 %% below plain %s", m, p)
	}
}

// A checkpoint whose check refuses it is rebuilt, and the tests under it, whose own checkpoints still match by
// key, pass all the same.
func TestSampleAFailedVerifyFallsBackWithoutFailingTests(t *testing.T) {
	s := newSample(t)
	g := s.rig("memo", "")
	s.commit(1)
	s.run(g, "memo-verify.spec.ts").mustPass(t, 4) // cold: builds and learns
	if err := os.WriteFile(s.marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	o := s.run(g, "memo-verify.spec.ts")
	o.mustPass(t, 4)
	if _, err := os.Stat(s.marker); err == nil {
		t.Fatal("the check never ran: the session was not restored")
	}
	rebuilt := false
	for _, f := range o.saved {
		rebuilt = rebuilt || strings.HasPrefix(f, "session")
	}
	if !rebuilt {
		t.Errorf("the refused session was not rebuilt: %v", o.saved)
	}
}

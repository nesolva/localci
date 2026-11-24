package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// The two-machine rig: a primary (store, suite runner, worker listener) and a worker with its own data directory,
// in one process, joined over the real TLS listener. The sample suite is the real Playwright package and the example
// shop app, so it needs `npm install` and `npm run build -w packages/playwright` at the repository root.

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

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

type rig struct {
	t       *testing.T
	st      *store.Store
	hub     *server.WorkerHub
	runner  *suite.Runner
	srv     *server.Server
	url, fp string // the worker listener
	dataA   string // the primary's data directory
	dataB   string // the worker's
	ctx     context.Context
}

// newRig sets a primary up for the sample shop with two shards: one here and one for a worker of capacity one.
func newRig(t *testing.T, slowMS string, lostAfter time.Duration) *rig {
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

	g := &rig{t: t, dataA: t.TempDir(), dataB: t.TempDir()}
	var cancel context.CancelFunc
	g.ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	settings := filepath.Join(t.TempDir(), "global-settings.json")
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
        PORT: '{{port "web"}}'
        DB_PATH: '{{db.url}}'
        ACME_GLOBAL_SETTINGS: %q
        SLOW_MS: %q
    shards: 2
    timeout: 10m
`, root, settings, slowMS)
	suites, err := suite.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if g.st, err = store.Open(filepath.Join(g.dataA, "store.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.st.Close() })

	g.hub = server.NewWorkerHub(lostAfter)
	g.hub.ClaimWait = time.Second
	// The runner's own API address (hooks of its local shards) is the primary's local handler, bound once it exists.
	var local http.Handler
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { local.ServeHTTP(w, r) }))
	t.Cleanup(api.Close)
	g.runner, err = suite.New(suite.Deps{Store: g.st, Log: event.NewLog(g.st, event.NewBus()), Sched: sched.New(3, 0), DataDir: g.dataA,
		Suites: suites, RepoURL: func(string) string { return src }, API: api.URL, Remote: g.hub})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.runner.Close)

	cert, fp, err := server.LoadWorkerCert(g.dataA, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.fp = fp
	g.srv = server.New(server.Deps{Store: g.st, Suites: g.runner, Workers: g.hub, WorkerCert: &cert, WorkerFingerprint: fp,
		GitRepos: []string{"acme/shop"}, DataDir: g.dataA, Token: "primary-token"})
	local = g.srv.Handler()
	wl, err := server.ListenWorker("local", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wl.Close() })
	go g.srv.ServeWorkers(g.ctx, wl)
	g.url = "https://" + wl.Addr().String()
	return g
}

// join gets a token and redeems it as name.
func (g *rig) join(name string) Config {
	g.t.Helper()
	tok, err := g.st.NewJoinToken(g.ctx)
	if err != nil {
		g.t.Fatal(err)
	}
	c, err := Join(g.ctx, g.url, tok, g.fp, name, 1)
	if err != nil {
		g.t.Fatal(err)
	}
	return c
}

// startWorker runs a joined worker until the returned cancel is called; done receives Run's error.
func (g *rig) startWorker(c Config) (stop context.CancelFunc, done <-chan error) {
	g.t.Helper()
	ctx, cancel := context.WithCancel(g.ctx)
	ch := make(chan error, 1)
	go func() {
		ch <- Run(ctx, Options{DataDir: g.dataB, Config: c, Capacity: 1, HeartbeatEvery: 300 * time.Millisecond, ClaimWait: time.Second})
	}()
	t := g.t
	t.Cleanup(cancel)
	// The primary offers shards only to workers it has heard from.
	for deadline := time.Now().Add(15 * time.Second); g.hub.Slots() < 1; {
		if time.Now().After(deadline) {
			t.Fatal("the worker never registered a free slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cancel, ch
}

func (g *rig) submit() string {
	g.t.Helper()
	id, err := g.runner.Submit(suite.Request{Suite: "shop", Heads: []suite.Head{{Head: forge.Head{Repo: "acme/shop", Branch: "main"}}}})
	if err != nil {
		g.t.Fatal(err)
	}
	return id
}

func (g *rig) wait(id string) store.Run {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Minute)
	defer cancel()
	run, err := g.runner.Wait(ctx, id)
	if err != nil {
		g.t.Fatal(err)
	}
	return run
}

func (g *rig) mustPass(run store.Run) []store.Test {
	g.t.Helper()
	tests, _ := g.st.RunTests(g.ctx, run.ID)
	if run.State != store.StatePassed {
		for _, x := range tests {
			g.t.Logf("%d %s %s retry %d %s", x.Shard, x.ID, x.Status, x.Retry, x.Failure)
		}
		g.t.Fatalf("run %s (%s)", run.State, run.Reason)
	}
	for _, x := range tests {
		if x.Status != "passed" {
			g.t.Errorf("%s is %s", x.ID, x.Status)
		}
	}
	return tests
}

func lanes(tests []store.Test) map[int]int {
	n := map[int]int{}
	for _, x := range tests {
		n[x.Shard]++
	}
	return n
}

// The sample shop split across a primary and a worker with its own data directory: both lanes run tests, every test
// passes, and the worker's files (mirror, workspace) are in its directory, not the primary's.
func TestTwoNodesRunTheSampleSplitAcrossBoth(t *testing.T) {
	g := newRig(t, "300", 0)
	g.startWorker(g.join("w1"))
	tests := g.mustPass(g.wait(g.submit()))
	if l := lanes(tests); l[1] == 0 || l[2] == 0 {
		t.Fatalf("tests per lane %v: both machines should have run some", l)
	}
	for _, d := range []string{"mirrors", "workspaces"} {
		if _, err := os.Stat(filepath.Join(g.dataB, d)); err != nil {
			t.Errorf("the worker kept no %s: %v", d, err)
		}
	}
	if ws, _ := g.st.Workers(g.ctx); len(ws) != 1 || ws[0].Name != "w1" {
		t.Errorf("workers: %+v", ws)
	}
}

// processesIn lists the live processes whose working directory is inside dir.
func processesIn(t *testing.T, dir string) []int {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := osexec.Command("lsof", "-d", "cwd", "-Fpn").Output()
	if err != nil && len(out) == 0 {
		t.Skipf("lsof is needed to find the shard's processes: %v", err)
	}
	var pids []int
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid != 0 && (line[1:] == real || strings.HasPrefix(line[1:], real+"/")):
			pids = append(pids, pid)
		}
	}
	return pids
}

// A worker that dies mid-shard loses it after the silence limit; its unfinished tests run again on the primary and
// the run still passes, with no test left running or retried.
func TestKilledWorkerShardIsRequeued(t *testing.T) {
	g := newRig(t, "1500", 2*time.Second)
	stop, done := g.startWorker(g.join("w1"))
	id := g.submit()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no test ever ran on the worker's shard")
		}
		ts, _ := g.st.RunTests(g.ctx, id)
		found := false
		for _, x := range ts {
			found = found || (x.Shard == 2 && x.Status == "running")
		}
		if found {
			break
		}
	}
	// The shard's app server is up now; it must go down with the worker.
	if len(processesIn(t, g.dataB)) == 0 {
		t.Fatal("no process of the shard is running in the worker's directory: the check below proves nothing")
	}
	stop()
	<-done
	if left := processesIn(t, g.dataB); len(left) != 0 {
		t.Errorf("processes of the shard outlived the worker: %v", left)
		for _, pid := range left {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	run := g.wait(id)
	tests := g.mustPass(run)
	for _, x := range tests {
		if x.Status == "running" || x.Retry != 0 {
			t.Errorf("%s: %s retry %d", x.ID, x.Status, x.Retry)
		}
	}
}

// A certificate that is not the pinned one stops the handshake before anything, the join token included, is sent;
// a revoked credential ends the worker with ErrRevoked.
func TestPinnedCertificateAndRevokedCredential(t *testing.T) {
	g := newRig(t, "300", 0)
	tok, _ := g.st.NewJoinToken(g.ctx)
	wrong := strings.Repeat("ab", 32)
	if _, err := Join(g.ctx, g.url, tok, wrong, "w1", 1); err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("join with a wrong fingerprint: %v", err)
	}
	// The token is still unused: the refused handshake never carried it.
	c, err := Join(g.ctx, g.url, tok, g.fp, "w1", 1)
	if err != nil {
		t.Fatalf("the token was spent by the refused handshake: %v", err)
	}
	_, done := g.startWorker(c)
	if err := g.st.RevokeWorker(g.ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("worker ended with %v, want ErrRevoked", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a revoked worker kept running")
	}
}

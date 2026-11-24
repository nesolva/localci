package suite

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__fake" {
		fakeMain(os.Args[2:])
		return
	}
	os.Setenv("FAKE_BIN", os.Args[0])
	os.Exit(m.Run())
}

// fakeSuite is a suite of the fake framework with a command database, one stack service and 2 shards;
// extra lines are added under the suite.
func fakeSuite(extra string) string {
	return `suites:
  e2e:
    repos: [acme/web]
    database:
      driver: command
      template: {seed: "echo seeded > {{db.url}}"}
      create: "cp {{template}} {{name}}"
      drop: "rm -f {{name}}"
    stack:
      - name: app
        repo: acme/web
        run: 'echo $$ >> "$FAKE_DIR/stack.pids"; echo up {{port "app"}}; exec sleep 300'
        ready: {log: "^up "}
    test:
      repo: acme/web
      run: '"$FAKE_BIN" __fake run'
      list: '"$FAKE_BIN" __fake list'
      env: {APP_PORT: '{{port "app"}}'}
    shards: 2
` + extra
}

type rig struct {
	t      *testing.T
	r      *Runner
	st     *store.Store
	sc     *sched.Scheduler
	data   string
	shared string // $FAKE_DIR
	src    string // the acme/web repository
	api    *httptest.Server
	gate   *gate
}

// newRig sets up a runner for the suites in yaml over a git repository holding tests.txt (one test id per line).
func newRig(t *testing.T, yaml string, tests []string, cpus int) *rig {
	t.Helper()
	g := &rig{t: t, data: t.TempDir(), shared: t.TempDir(), src: t.TempDir()}
	t.Setenv("FAKE_DIR", g.shared)
	git(t, g.src, "init", "-q", "-b", "main")
	g.commit(map[string]string{"tests.txt": strings.Join(tests, "\n") + "\n", "lock.txt": "1\n"})

	var err error
	if g.st, err = store.Open(filepath.Join(g.data, "store.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.st.Close() })
	suites, err := ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	g.sc = sched.New(cpus, 0)
	g.r, err = New(Deps{Store: g.st, Log: event.NewLog(g.st, event.NewBus()), Sched: g.sc, DataDir: g.data, Suites: suites,
		RepoURL: func(string) string { return g.src }})
	if err != nil {
		t.Fatal(err)
	}
	g.gate = hookGate(g.r)
	g.api = httptest.NewServer(g.gate)
	g.r.d.API = g.api.URL
	t.Cleanup(func() { g.r.Close(); g.api.Close() })
	return g
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (g *rig) commit(files map[string]string) {
	g.t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(g.src, name), []byte(body), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	git(g.t, g.src, "add", "-A")
	git(g.t, g.src, "commit", "-q", "-m", "c")
}

func mainHead() []Head { return []Head{{Head: forge.Head{Repo: "acme/web", Branch: "main"}}} }

func prHead(pr int) []Head {
	return []Head{{Head: forge.Head{Repo: "acme/web", Branch: "main", PR: pr}}}
}

// run submits and waits for the run to end.
func (g *rig) run(req Request) store.Run {
	g.t.Helper()
	id, err := g.r.Submit(req)
	if err != nil {
		g.t.Fatal(err)
	}
	return g.wait(id)
}

func (g *rig) wait(id string) store.Run {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	run, err := g.r.Wait(ctx, id)
	if err != nil {
		g.t.Fatalf("run %s: %v", id, err)
	}
	return run
}

// dbFiles lists the shard databases the command driver has on disk (the template excluded).
func (g *rig) dbFiles() []string {
	var out []string
	ents, _ := os.ReadDir(filepath.Join(g.data, "state", "e2e", "db"))
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "localci_tpl_") {
			out = append(out, e.Name())
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alive reports whether pid is a running process. A zombie counts as gone: in a
// container with no init process nothing reaps orphans, so they stay listed
// as zombies although they have ended.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}

func pidsIn(path string) []int {
	var out []int
	for _, l := range readLines(path) {
		var p int
		if _, err := fmt.Sscan(l, &p); err == nil && p > 0 {
			out = append(out, p)
		}
	}
	return out
}

// hookGate is the server's hook check without the server (which imports this package): a hook call must carry the
// token of its run, so every test that goes through it proves the shards send theirs. It counts what it let
// through and what it refused.
type gate struct {
	http.Handler
	passed, refused atomic.Int64
}

func hookGate(r *Runner) *gate {
	g := &gate{}
	g.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/runs/"), "/")
		if len(parts) < 4 || !r.HookTokenValid(parts[0], strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")) {
			g.refused.Add(1)
			http.Error(w, "hook token", http.StatusForbidden)
			return
		}
		g.passed.Add(1)
		r.Handler().ServeHTTP(w, req)
	})
	return g
}

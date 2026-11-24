package exec

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func logPath(t *testing.T) string { return filepath.Join(t.TempDir(), "run.log") }

func collect(ctx context.Context, p *Proc) []string {
	var lines []string
	for l := range p.Subscribe(ctx) {
		lines = append(lines, l)
	}
	return lines
}

func TestProcessOutputExitAndEnv(t *testing.T) {
	dir := t.TempDir()
	p, err := Start(bg, Spec{
		Cmd:     `echo "in $PWD: $GREETING"; echo oops >&2; exit 3`,
		Dir:     dir,
		Env:     []string{"GREETING=hello"},
		LogPath: logPath(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	res := p.Wait()
	if res.ExitCode != 3 || res.OK() || res.TimedOut || res.Cancelled {
		t.Fatalf("result %+v", res)
	}
	// A subscriber that joins after the process ended still gets the whole output, in order.
	lines := collect(bg, p)
	real, _ := filepath.EvalSymlinks(dir)
	if len(lines) != 2 || !strings.HasSuffix(lines[0], ": hello") || !strings.Contains(lines[0], filepath.Base(real)) || lines[1] != "oops" {
		t.Fatalf("lines %q", lines)
	}
	if b, _ := os.ReadFile(p.LogPath()); string(b) != lines[0]+"\noops\n" {
		t.Fatalf("log file %q", b)
	}
}

func TestProcessStreamsWhileRunning(t *testing.T) {
	p, err := Start(bg, Spec{Cmd: `echo first; read -t 5 line; echo "got $line"`, LogPath: logPath(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	ch := p.Subscribe(ctx)
	if l := <-ch; l != "first" {
		t.Fatalf("first line %q", l)
	}
	p.Cancel() // the subscriber saw the line while the process was still running
	p.Wait()
}

func TestProcessPipefail(t *testing.T) {
	res, err := Run(bg, Spec{Cmd: `false | true; echo unreachable`, LogPath: logPath(t)})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 {
		t.Fatal("a failing pipeline stage must fail the command (bash -eo pipefail)")
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

// pidsGone reports whether both pids listed in file are gone within a few seconds.
func pidsGone(t *testing.T, file string) bool {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pids := strings.Fields(string(b))
	if len(pids) != 2 {
		t.Fatalf("pid file: %q", b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		gone := true
		for _, s := range pids {
			pid, _ := strconv.Atoi(s)
			if alive(pid) {
				gone = false
			}
		}
		if gone {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestKillGroupOnTimeoutCancelAndStubbornChildren(t *testing.T) {
	// Two grandchildren in the group; the second ignores SIGTERM.
	script := func(pids, tail string) string {
		return fmt.Sprintf(`sleep 300 & echo -n "$! " > %[1]s; (trap '' TERM; exec sleep 301) & echo $! >> %[1]s; %s`, pids, tail)
	}
	pause := func() { time.Sleep(400 * time.Millisecond) }
	cases := []struct {
		name  string
		spec  func(pids string) Spec
		stop  func(p *Proc, cancel context.CancelFunc)
		check func(t *testing.T, r Result)
	}{
		{"timeout", func(pids string) Spec {
			return Spec{Cmd: script(pids, "wait"), Timeout: 400 * time.Millisecond, KillGrace: 300 * time.Millisecond}
		}, func(*Proc, context.CancelFunc) {}, func(t *testing.T, r Result) {
			if !r.TimedOut || r.Cancelled {
				t.Fatalf("%+v", r)
			}
		}},
		{"cancel context", func(pids string) Spec {
			return Spec{Cmd: script(pids, "wait"), KillGrace: 300 * time.Millisecond}
		}, func(_ *Proc, cancel context.CancelFunc) { pause(); cancel() }, func(t *testing.T, r Result) {
			if !r.Cancelled || r.TimedOut {
				t.Fatalf("%+v", r)
			}
		}},
		{"cancel method", func(pids string) Spec {
			return Spec{Cmd: script(pids, "wait"), KillGrace: 300 * time.Millisecond}
		}, func(p *Proc, _ context.CancelFunc) { pause(); p.Cancel() }, func(t *testing.T, r Result) {
			if !r.Cancelled {
				t.Fatalf("%+v", r)
			}
		}},
		{"background jobs left behind by a command that succeeds", func(pids string) Spec {
			return Spec{Cmd: script(pids, "true"), KillGrace: 300 * time.Millisecond}
		}, func(*Proc, context.CancelFunc) {}, func(t *testing.T, r Result) {
			if !r.OK() {
				t.Fatalf("%+v", r)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pids := filepath.Join(t.TempDir(), "pids")
			spec := c.spec(pids)
			spec.LogPath = logPath(t)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			p, err := Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			c.stop(p, cancel)
			c.check(t, p.Wait())
			if !pidsGone(t, pids) {
				t.Fatal("a process of the group is still alive")
			}
		})
	}
}

func TestSamplesProcessTree(t *testing.T) {
	var mu sync.Mutex
	var got []Sample
	res, err := Run(bg, Spec{
		Cmd: `sleep 1 & sleep 1 & wait`, LogPath: logPath(t), SampleEvery: 200 * time.Millisecond,
		OnSample: func(s Sample) { mu.Lock(); got = append(got, s); mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || res.PeakRSSKB == 0 {
		t.Fatalf("no samples: %d, peak %d", len(got), res.PeakRSSKB)
	}
	if got[0].Procs < 3 { // bash and its two children
		t.Fatalf("tree not followed: %+v", got[0])
	}
}

func TestTreeStatsFollowsDescendantsAndOrphans(t *testing.T) {
	rows := []psRow{
		{pid: 10, ppid: 1, pgid: 10, cpu: 1, rssKB: 100},   // root
		{pid: 11, ppid: 10, pgid: 10, cpu: 2, rssKB: 200},  // child
		{pid: 12, ppid: 11, pgid: 12, cpu: 4, rssKB: 400},  // grandchild in its own group
		{pid: 13, ppid: 1, pgid: 10, cpu: 8, rssKB: 800},   // orphan still in the group
		{pid: 99, ppid: 1, pgid: 99, cpu: 16, rssKB: 1600}, // unrelated
	}
	s := treeStats(rows, 10)
	if s.Procs != 4 || s.RSSKB != 1500 || s.CPU != 15 {
		t.Fatalf("%+v", s)
	}
}

func TestRememberedIgnoresReusedPid(t *testing.T) {
	var r remembered
	r.add([]psRow{
		{pid: 10, ppid: 1, pgid: 10, start: "Mon Nov 24 10:00:00 2026"},
		{pid: 11, ppid: 10, pgid: 11, start: "Mon Nov 24 10:00:01 2026"},
	}, 10)
	// pid 11 exited and was reused by an unrelated launchd-started process.
	now := []psRow{
		{pid: 10, ppid: 1, pgid: 10, start: "Mon Nov 24 10:00:00 2026"},
		{pid: 11, ppid: 1, pgid: 11, start: "Mon Nov 24 11:30:00 2026"},
	}
	got := r.ours(now)
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("ours = %v, want only pid 10", got)
	}
}

// TestHelperServer is not a test: the test binary re-executes itself as a tiny
// server for the service tests.
func TestHelperServer(t *testing.T) {
	if os.Getenv("LOCALCI_HELPER") != "server" {
		t.Skip("helper process")
	}
	time.Sleep(300 * time.Millisecond) // not instantly ready
	fmt.Println("listening")
	http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

func serverSpec(t *testing.T, port int) Spec {
	exe, _ := os.Executable()
	return Spec{
		Cmd:     fmt.Sprintf("exec %q -test.run=TestHelperServer", exe),
		Env:     []string{"LOCALCI_HELPER=server", "PORT=" + strconv.Itoa(port)},
		LogPath: logPath(t),
	}
}

func listening(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestServiceReadyProbes(t *testing.T) {
	for _, name := range []string{"http", "tcp", "log"} {
		t.Run(name, func(t *testing.T) {
			ports, err := FreePorts(1)
			if err != nil {
				t.Fatal(err)
			}
			defer ReleasePorts(ports...)
			port := ports[0]
			ready := map[string]Ready{
				"http": {HTTP: fmt.Sprintf("http://127.0.0.1:%d/health", port), Timeout: 10 * time.Second},
				"tcp":  {TCP: port, Timeout: 10 * time.Second},
				"log":  {Log: `^listening$`, Timeout: 10 * time.Second},
			}[name]
			start := time.Now()
			svc, err := StartService(bg, serverSpec(t, port), ready)
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) < 250*time.Millisecond {
				t.Fatal("reported ready before the server could answer")
			}
			svc.Stop()
			if listening(port) {
				t.Fatal("service still listening after Stop")
			}
		})
	}
}

func TestServiceEarlyExitAndNeverReadyReportLog(t *testing.T) {
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, fmt.Sprintf("echo line %d", i))
	}
	_, err := StartService(bg, Spec{Cmd: strings.Join(lines, "; ") + "; echo address in use >&2; exit 2", LogPath: logPath(t)},
		Ready{HTTP: "http://127.0.0.1:1/", Timeout: 10 * time.Second})
	se, ok := err.(*ServiceError)
	if !ok {
		t.Fatalf("want *ServiceError, got %v", err)
	}
	if !strings.Contains(se.Reason, "exited early") || !strings.Contains(se.Reason, "2") {
		t.Fatalf("reason %q", se.Reason)
	}
	if len(se.Lines) != 20 || se.Lines[19] != "address in use" || se.Lines[0] != "line 7" {
		t.Fatalf("want the last 20 lines, got %d: %q", len(se.Lines), se.Lines)
	}

	start := time.Now()
	_, err = StartService(bg, Spec{Cmd: `echo starting; exec sleep 30`, LogPath: logPath(t), KillGrace: 200 * time.Millisecond},
		Ready{Log: `never printed`, Timeout: 500 * time.Millisecond})
	se, ok = err.(*ServiceError)
	if !ok || !strings.Contains(se.Reason, "not ready") || len(se.Lines) != 1 || se.Lines[0] != "starting" {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a service that never gets ready must be stopped at the timeout")
	}
}

func TestPortsConcurrentCallersNeverShare(t *testing.T) {
	const callers, each = 24, 6
	var (
		mu   sync.Mutex
		seen = map[int]int{}
		wg   sync.WaitGroup
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ports, err := FreePorts(each)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			for _, p := range ports {
				seen[p]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != callers*each {
		t.Fatalf("expected %d distinct ports, got %d", callers*each, len(seen))
	}
	for p := range seen {
		ReleasePorts(p)
	}
}

// A tool may start its server in a group of its own (job control here, a detached child in Playwright's case), out of
// reach of the group's signal. Whatever the command started still ends with it when it is cancelled or times out.
func TestProcessEndsDescendantsThatLeftTheGroup(t *testing.T) {
	for _, c := range []struct {
		name string
		spec Spec
		stop func(p *Proc)
	}{
		{"cancelled", Spec{}, func(p *Proc) { time.Sleep(400 * time.Millisecond); p.Cancel() }},
		{"timed out", Spec{Timeout: 400 * time.Millisecond}, func(*Proc) {}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pids := filepath.Join(t.TempDir(), "pids")
			c.spec.Cmd = fmt.Sprintf(`set -m; sleep 302 & echo -n "$! " > %[1]s; sleep 303 & echo $! >> %[1]s; wait`, pids)
			c.spec.LogPath, c.spec.KillGrace = logPath(t), 300*time.Millisecond
			p, err := Start(bg, c.spec)
			if err != nil {
				t.Fatal(err)
			}
			c.stop(p)
			p.Wait()
			if !pidsGone(t, pids) {
				t.Fatal("a process started by the command outlived it")
			}
		})
	}
}

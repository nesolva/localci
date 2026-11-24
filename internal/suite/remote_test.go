package suite

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRemote is a worker pool made of the fake framework: RunShard runs the fake test command for the job's tests and
// sends its report to the sink, or follows script.
type fakeRemote struct {
	t      *testing.T
	slots  int
	script func(call int, job RemoteJob, sink RemoteSink) (RemoteResult, error)
	mu     sync.Mutex
	jobs   []RemoteJob
}

func (f *fakeRemote) Slots() int { return f.slots }

func (f *fakeRemote) RunShard(ctx context.Context, job RemoteJob, sink RemoteSink) (RemoteResult, error) {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	call := len(f.jobs)
	f.mu.Unlock()
	if f.script != nil {
		return f.script(call, job, sink)
	}
	return f.runFake(job, sink)
}

func (f *fakeRemote) runFake(job RemoteJob, sink RemoteSink) (RemoteResult, error) {
	dir := f.t.TempDir()
	os.WriteFile(filepath.Join(dir, "tests"), []byte(strings.Join(job.Tests, "\n")+"\n"), 0o644)
	cmd := osexec.Command(os.Getenv("FAKE_BIN"), "__fake", "run")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOCALCI_REPORT="+filepath.Join(dir, "report"), "LOCALCI_TESTS="+filepath.Join(dir, "tests"),
		fmt.Sprintf("LOCALCI_SHARD=%d", job.Shard), "LOCALCI_OUTPUT_DIR="+filepath.Join(dir, "out"))
	err := cmd.Run()
	b, _ := os.ReadFile(filepath.Join(dir, "report"))
	if err := sink.Report(b); err != nil {
		return RemoteResult{}, err
	}
	code := 0
	if err != nil {
		code = 1
	}
	return RemoteResult{ExitCode: code}, nil
}

func (f *fakeRemote) jobsSnapshot() []RemoteJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.jobs)
}

func manyTests() []string { return []string{"pass1", "pass2", "pass3", "pass4", "pass5", "pass6"} }

// A fork's code runs on this machine unless workers.forks says another machine may run it; a pull request of this
// repository goes to a worker.
func TestForkRunStaysLocalUnlessWorkersMayTakeForks(t *testing.T) {
	for _, c := range []struct {
		name       string
		fork, open bool
		wantRemote bool
	}{
		{"own branch", false, false, true},
		{"fork", true, false, false},
		{"fork with workers.forks", true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeSuite(""), manyTests(), 4)
			fr := &fakeRemote{t: t, slots: 1}
			g.r.d.Remote, g.r.d.RemoteForks = fr, c.open
			run := g.run(Request{Suite: "e2e", Heads: mainHead(), Fork: c.fork})
			g.mustPass(run)
			if got := len(fr.jobsSnapshot()) > 0; got != c.wantRemote {
				t.Fatalf("a worker got a shard: %v, want %v", got, c.wantRemote)
			}
			seen := map[string]bool{}
			for _, a := range g.attempts(run.ID) {
				seen[a.Test] = true
			}
			if len(seen) != len(manyTests()) {
				t.Fatalf("tests stored: %v", seen)
			}
		})
	}
}

// A worker that goes silent mid-shard gives back only the tests it did not finish: another worker or this machine
// runs them, the run does not end infra, and no test keeps a stale running row or a retry number.
func TestLostWorkerRequeuesWhatItDidNotReport(t *testing.T) {
	for _, c := range []struct {
		name string
		next func(f *fakeRemote, job RemoteJob, sink RemoteSink) (RemoteResult, error)
	}{
		{"another worker takes it", func(f *fakeRemote, job RemoteJob, sink RemoteSink) (RemoteResult, error) { return f.runFake(job, sink) }},
		{"nobody does, it runs here", func(*fakeRemote, RemoteJob, RemoteSink) (RemoteResult, error) { return RemoteResult{}, ErrNoWorker }},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t, fakeSuite(""), manyTests(), 4)
			fr := &fakeRemote{t: t, slots: 1}
			fr.script = func(call int, job RemoteJob, sink RemoteSink) (RemoteResult, error) {
				if call > 1 {
					return c.next(fr, job, sink)
				}
				now := time.Now().UnixMilli()
				sink.Report([]byte(fmt.Sprintf(`{"v":1,"t":"begin","ts":%d,"shard":1,"total":3}`+"\n"+
					`{"v":1,"t":"test.begin","ts":%d,"test":%q,"file":"f","title":"t","line":1,"retry":0}`+"\n"+
					`{"v":1,"t":"test.end","ts":%d,"test":%q,"status":"passed","ms":5,"retry":0}`+"\n"+
					`{"v":1,"t":"test.begin","ts":%d,"test":%q,"file":"f","title":"t","line":1,"retry":0}`+"\n",
					now, now, job.Tests[0], now, job.Tests[0], now, job.Tests[1])))
				time.Sleep(300 * time.Millisecond) // the tail picks the lines up before the worker is given up on
				return RemoteResult{}, ErrWorkerLost
			}
			g.r.d.Remote = fr
			run := g.run(Request{Suite: "e2e", Heads: mainHead()})
			g.mustPass(run)
			jobs := fr.jobsSnapshot()
			if len(jobs) < 2 {
				t.Fatalf("shard was not offered again: %d jobs", len(jobs))
			}
			if jobs[1].Gen != 2 || slices.Contains(jobs[1].Tests, jobs[0].Tests[0]) || !slices.Contains(jobs[1].Tests, jobs[0].Tests[1]) {
				t.Fatalf("requeue gave %v after %v (gen %d)", jobs[1].Tests, jobs[0].Tests, jobs[1].Gen)
			}
			final := last(g.attempts(run.ID))
			if len(final) != len(manyTests()) {
				t.Fatalf("tests stored: %v", final)
			}
			for id, a := range final {
				if a.Status != "passed" || a.Retry != 0 {
					t.Errorf("%s: %s retry %d, want passed on the first attempt", id, a.Status, a.Retry)
				}
			}
			for _, a := range g.attempts(run.ID) {
				if a.Status == "running" {
					t.Errorf("%s keeps a stale running row", a.Test)
				}
			}
		})
	}
}

// What a worker sends as artifacts is untrusted: an entry that leaves the directory, a link, or too much data is
// refused, and nothing is written outside the shard's directory.
func TestArtifactArchiveIsConfinedToTheShardDirectory(t *testing.T) {
	build := func(h tar.Header, body string) *bytes.Buffer {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		h.Size = int64(len(body))
		w.WriteHeader(&h)
		w.Write([]byte(body))
		w.Close()
		return &b
	}
	root := t.TempDir()
	dir := filepath.Join(root, "shard.out")
	for _, c := range []struct {
		name string
		h    tar.Header
	}{
		{"parent path", tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644}},
		{"absolute path", tar.Header{Name: filepath.Join(root, "abs.txt"), Typeflag: tar.TypeReg, Mode: 0o644}},
		{"symlink", tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}},
	} {
		if err := UntarConfined(build(c.h, "x"), dir, 1<<20, 10); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	if err := UntarConfined(build(tar.Header{Name: "a.bin", Typeflag: tar.TypeReg, Mode: 0o644}, strings.Repeat("x", 100)), dir, 50, 10); err == nil {
		t.Error("an archive over the size cap was accepted")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Errorf("something was written outside the shard directory: %v", ents)
	}
	if err := UntarConfined(build(tar.Header{Name: "t1/shot.png", Typeflag: tar.TypeReg, Mode: 0o644}, "png"), dir, 1<<20, 10); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "t1", "shot.png")); string(b) != "png" {
		t.Errorf("artifact not unpacked: %q", b)
	}
}

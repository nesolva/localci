package suite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// The worker side of remote shards. A WorkerRunner prepares the run's workspace once (checkout from the primary's
// mirror, prepare, database template), then runs each shard it is given with a database and stack of its own, and
// hands the report, the log and the artifacts to a WorkerOutput. It never ingests a report: the primary does.

// WorkerOutput is where a worker ships what a shard produces.
type WorkerOutput interface {
	// Report sends whole report lines, in order; Log sends test command output.
	Report(p []byte) error
	Log(p []byte) error
	// Artifacts sends the files under dir (which may not exist).
	Artifacts(dir string) error
}

// WorkerRunner runs shards for a primary on this machine.
type WorkerRunner struct {
	r    *Runner
	mu   sync.Mutex
	runs map[string]*workerRun
}

type workerRun struct {
	ex     *execution
	cancel context.CancelCauseFunc
	refs   int
	once   sync.Once
	err    error
}

// NewWorkerRunner makes a runner whose state (mirrors, workspaces, databases, logs) lives under d.DataDir. d.API is the
// base URL this machine's tests reach the primary's hooks at (the loopback relay); d.RepoURL says where repositories
// are fetched from (the relay's git endpoint).
func NewWorkerRunner(d Deps) (*WorkerRunner, error) {
	d.Remote = nil
	r, err := New(d)
	if err != nil {
		return nil, err
	}
	return &WorkerRunner{r: r, runs: map[string]*workerRun{}}, nil
}

// Close stops what the runner started.
func (w *WorkerRunner) Close() { w.r.Close() }

func (w *WorkerRunner) acquire(rj RemoteJob) (*workerRun, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if wr := w.runs[rj.Run]; wr != nil {
		wr.refs++
		return wr, nil
	}
	suites, err := FromConfig(map[string]any{rj.Suite: rj.Config})
	if err != nil || len(suites) != 1 {
		return nil, fmt.Errorf("suite %s: %v", rj.Suite, err)
	}
	s := suites[0]
	var heads []Head
	for _, h := range rj.Heads {
		heads = append(heads, Head{Head: forge.Head{Repo: h.Repo, SHA: h.SHA}})
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	j := &job{id: rj.Run, suite: s, req: Request{Suite: s.Name, Heads: heads}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	ex := &execution{r: w.r, j: j, suite: s, id: rj.Run, ctx: ctx, bg: context.Background(), cancel: cancel,
		attempts: map[string]int{}, envOf: map[string]*shardEnv{}, laneOf: map[string]int{}, dirs: map[string]string{},
		mirrors: map[string]*exec.Mirror{}, shas: map[string]string{}, hookToken: rj.HookToken}
	ex.logDir = filepath.Join(w.r.d.DataDir, "logs", rj.Run)
	ex.state = filepath.Join(w.r.d.DataDir, "state", slug(s.Name))
	if err := os.MkdirAll(ex.logDir, 0o755); err != nil {
		cancel(nil)
		return nil, err
	}
	if err := w.r.d.Store.PutRun(ctx, store.Run{ID: rj.Run, Suite: s.Name, Kind: store.KindSuite, State: store.StateRunning}); err != nil {
		cancel(nil)
		return nil, err
	}
	wr := &workerRun{ex: ex, cancel: cancel, refs: 1}
	w.runs[rj.Run] = wr
	return wr, nil
}

func (w *WorkerRunner) release(run string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wr := w.runs[run]
	if wr == nil {
		return
	}
	if wr.refs--; wr.refs <= 0 {
		wr.cancel(nil)
		delete(w.runs, run)
	}
}

// RunShard runs one shard of a run for the primary and returns how the test command ended. A setup failure is
// reported in the result, not as an error; the error is for a shard that was cancelled.
func (w *WorkerRunner) RunShard(ctx context.Context, rj RemoteJob, out WorkerOutput) (RemoteResult, error) {
	wr, err := w.acquire(rj)
	if err != nil {
		return RemoteResult{Error: err.Error()}, nil
	}
	defer w.release(rj.Run)
	ex := wr.ex
	wr.once.Do(func() {
		for _, step := range []func() error{ex.checkout, ex.prepare, ex.setupDB} {
			if wr.err = step(); wr.err != nil {
				return
			}
		}
	})
	if wr.err != nil {
		return RemoteResult{Error: trim(wr.err.Error(), 600)}, nil
	}
	ids := make([]shard.TestID, len(rj.Tests))
	for i, t := range rj.Tests {
		ids[i] = shard.TestID(t)
	}
	e, err := ex.openEnv(ctx, rj.Shard, ids)
	if err != nil {
		if ctx.Err() != nil {
			return RemoteResult{}, ctx.Err()
		}
		return RemoteResult{Error: trim(err.Error(), 600)}, nil
	}
	defer e.close()
	is, err := e.buildInvoke(ids, fmt.Sprintf(".w%d", rj.Gen))
	if err != nil {
		return RemoteResult{Error: err.Error()}, nil
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var shipErr error
	var emu sync.Mutex
	onErr := func(err error) {
		emu.Lock()
		if shipErr == nil {
			shipErr = err
		}
		emu.Unlock()
		cancel() // the primary stopped listening: nothing this shard does matters
	}
	rewrite := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte(is.outDir), []byte(rj.OutDir)) }
	stopReport := shipFile(sctx, is.reportFile, true, func(b []byte) error { return out.Report(rewrite(b)) }, onErr)
	stopLog := shipFile(sctx, is.spec.LogPath, false, out.Log, onErr)
	p, err := exec.Start(sctx, is.spec)
	if err != nil {
		stopReport()
		stopLog()
		return RemoteResult{Error: err.Error()}, nil
	}
	res := p.Wait()
	stopReport()
	stopLog()
	e.measure(res)
	emu.Lock()
	err = shipErr
	emu.Unlock()
	if err == nil {
		err = out.Artifacts(is.outDir)
	}
	e.close()
	cost, cpu := e.cost()
	if err != nil {
		return RemoteResult{}, err
	}
	if ctx.Err() != nil {
		return RemoteResult{}, ctx.Err()
	}
	return RemoteResult{ExitCode: res.ExitCode, RSSKB: int64(cost.Memory / 1024), CPUPct: cpu * 100}, nil
}

// shipFile sends what is appended to path, as it grows: whole lines when whole is set (the report), anything
// otherwise. The returned stop sends the rest and returns once everything written has been sent.
func shipFile(ctx context.Context, path string, whole bool, send func([]byte) error, onErr func(error)) (stop func()) {
	done, quit := make(chan struct{}), make(chan struct{})
	var off int64
	flush := func(final bool) bool {
		f, err := os.Open(path)
		if err != nil {
			return true
		}
		defer f.Close()
		if _, err := f.Seek(off, 0); err != nil {
			return true
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(f); err != nil || buf.Len() == 0 {
			return true
		}
		b := buf.Bytes()
		if whole && !final {
			if i := bytes.LastIndexByte(b, '\n'); i < 0 {
				return true
			} else {
				b = b[:i+1]
			}
		}
		if err := send(b); err != nil {
			onErr(err)
			return false
		}
		off += int64(len(b))
		return true
	}
	go func() {
		defer close(done)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if !flush(false) {
					return
				}
			case <-quit:
				flush(true)
				return
			}
		}
	}()
	return func() { close(quit); <-done }
}

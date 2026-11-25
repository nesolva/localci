package suite

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// Remote shards. A run still plans, prepares and lists on this machine; the parallel shards past the local ones are
// handed to workers on other machines, which run the suite's test command in their own workspace, database and stack
// and send the report lines, the log and the artifacts back. This machine ingests the report exactly as it does for
// a local shard, so a remote run looks like a local one. The serial group, the reruns and the probes never leave it.

var (
	// ErrNoWorker is returned by Remote.RunShard when no worker took the shard in time; the shard runs here.
	ErrNoWorker = errors.New("no worker took the shard")
	// ErrWorkerLost is returned when the worker went silent (or was revoked) before it finished the shard.
	ErrWorkerLost = errors.New("the worker was lost")
)

// RemoteHead is one repository at the commit under test, resolved on this machine.
type RemoteHead struct {
	Repo string `json:"repo"`
	SHA  string `json:"sha"`
}

// RemoteJob is one shard of one run, as a worker receives it.
type RemoteJob struct {
	Run   string `json:"run"`
	Suite string `json:"suite"`
	Shard int    `json:"shard"`
	// Gen tells assignments of the same shard apart (a requeue is a new generation): messages of an old one are refused.
	Gen    int            `json:"gen"`
	Config map[string]any `json:"config"` // the suite's own mapping
	Heads  []RemoteHead   `json:"heads"`
	Tests  []string       `json:"tests"`
	// HookToken is the run's hook token: the worker's tests send it with every hook call.
	HookToken string `json:"hook_token"`
	// OutDir is where this machine keeps the shard's artifacts; the worker writes it into the report lines that name one.
	OutDir string `json:"out_dir"`
}

// RemoteResult is how the worker's test command ended.
type RemoteResult struct {
	ExitCode int     `json:"exit"`
	RSSKB    int64   `json:"rss_kb"`
	CPUPct   float64 `json:"cpu_pct"`
	// Error is set when the worker could not run the shard at all (checkout, prepare, database or stack failed).
	Error string `json:"error,omitempty"`
}

// RemoteSink is where a worker's shard output lands on this machine.
type RemoteSink interface {
	// Report appends whole report lines.
	Report(p []byte) error
	// Log appends test command output.
	Log(p []byte) error
	// Artifacts unpacks a tar of the shard's artifact directory into this machine's copy.
	Artifacts(r io.Reader) error
}

// Remote hands shards to workers. The server package implements it.
type Remote interface {
	// Slots is how many shards the connected workers can take at once right now.
	Slots() int
	// RunShard gives the shard to a worker and blocks until the worker is done with it, or ErrNoWorker / ErrWorkerLost.
	RunShard(ctx context.Context, job RemoteJob, sink RemoteSink) (RemoteResult, error)
}

// remoteShards says how many of the run's shards the workers take: the ones past the local ones.
func (ex *execution) remoteSlots() int {
	d := ex.r.d
	switch {
	case d.Remote == nil, ex.j.req.Explore != nil, ex.iso != nil, ex.suite.Isolation != "isolated" && ex.suite.Isolation != "":
		return 0
	case ex.fork() && !d.RemoteForks:
		return 0
	}
	return max(d.Remote.Slots(), 0)
}

func (ex *execution) markRemote(ids []shard.TestID, on bool) {
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if ex.remote == nil {
		ex.remote = map[string]bool{}
	}
	for _, id := range ids {
		if on {
			ex.remote[string(id)] = true
		} else {
			delete(ex.remote, string(id))
		}
	}
}

// maxRemoteTries bounds how many workers one shard is given before this machine runs it.
const maxRemoteTries = 3

// runRemoteShard runs shard n on a worker. A worker that is lost mid-shard gives back only what it did not
// report: those tests go to another worker, or run here, and the run does not end infra because of it.
func (ex *execution) runRemoteShard(n int, ids []shard.TestID) error {
	log := filepath.Join(ex.logDir, fmt.Sprintf("shard-%d.log", n))
	ex.putShard(n, store.StateRunning, log, sched.Cost{}, 0)
	var cfg map[string]any
	if ex.suite.Raw != nil {
		ex.suite.Raw.Decode(&cfg)
	}
	ex.mu.Lock()
	var heads []RemoteHead
	for repo, sha := range ex.shas {
		heads = append(heads, RemoteHead{Repo: repo, SHA: sha})
	}
	ex.mu.Unlock()
	slices.SortFunc(heads, func(a, b RemoteHead) int { return strings.Compare(a.Repo, b.Repo) })

	pending := slices.Clone(ids)
	var runErr error
	var cost sched.Cost
	var cpu float64
	failed := false
	for gen := 1; len(pending) > 0 && runErr == nil; gen++ {
		ex.markRemote(pending, true)
		base := fmt.Sprintf("shard-%d.remote%d", n, gen)
		sink := &shardSink{reportFile: filepath.Join(ex.logDir, base+".ndjson"), logFile: filepath.Join(ex.logDir, base+".log"),
			outDir: filepath.Join(ex.logDir, base+".out")}
		if err := os.WriteFile(filepath.Join(ex.logDir, base+".tests"), []byte(joinIDs(pending)), 0o644); err != nil {
			runErr = err
			break
		}
		job := RemoteJob{Run: ex.id, Suite: ex.suite.Name, Shard: n, Gen: gen, Config: cfg, Heads: heads, Tests: idStrings(pending),
			HookToken: ex.hookToken, OutDir: sink.outDir}
		ing := newIngest(ex, n, modeParallel)
		stopTail, tailDone := ing.follow(sink.reportFile)
		res, err := ex.r.d.Remote.RunShard(ex.ctx, job, sink)
		sink.close()
		stopTail()
		<-tailDone
		switch {
		case ex.ctx.Err() != nil:
			runErr = ex.ctx.Err()
		case err == nil && res.Error != "":
			runErr = fmt.Errorf("shard %d on a worker: %s", n, trim(res.Error, 400))
			failed = true
		case err == nil:
			crashed := ing.finish(exec.Result{ExitCode: res.ExitCode}, pending)
			cost, cpu = sched.Cost{Memory: uint64(res.RSSKB) * 1024}, res.CPUPct/100
			failed = failed || crashed || ex.laneFailed(n)
			if crashed {
				runErr = errors.New(crashReason(n, exec.Result{ExitCode: res.ExitCode}))
			}
			pending = nil
		case errors.Is(err, ErrWorkerLost) || errors.Is(err, ErrNoWorker):
			pending = ing.abandon(pending)
			ex.emit(n, "shard.requeued", "", "", map[string]any{"reason": err.Error(), "tests": len(pending)})
			if len(pending) > 0 && (errors.Is(err, ErrNoWorker) || gen >= maxRemoteTries) {
				var c sched.Cost
				var p float64
				c, p, failed, runErr = ex.runRequeued(n, pending, gen)
				cost, cpu, pending = c, p, nil
			}
		default:
			runErr, failed = err, true
		}
	}
	ex.markRemote(ids, false)
	state := store.StatePassed
	if runErr != nil || failed || ex.laneFailed(n) {
		state = store.StateFailed
	}
	ex.putShard(n, state, log, cost, cpu)
	return runErr
}

// runRequeued runs tests no worker could finish on this machine, inside the run's own admitted grant: it waits for
// one of the run's slots, like a local shard, and never asks the scheduler again (the run holds a grant that may
// already fill the machine, so a second admission would wait for the run itself).
func (ex *execution) runRequeued(n int, ids []shard.TestID, gen int) (cost sched.Cost, cpu float64, failed bool, err error) {
	release, err := ex.takeSlot()
	if err != nil {
		return cost, cpu, true, err
	}
	defer release()
	ex.markRemote(ids, false)
	e, err := ex.openEnv(ex.ctx, n, ids)
	if err != nil {
		return cost, cpu, true, fmt.Errorf("shard %d: %w", n, err)
	}
	defer e.close()
	inv, err := e.invoke(ex.ctx, ids, modeParallel, fmt.Sprintf(".requeue%d", gen))
	e.close()
	cost, cpu = e.cost()
	if err != nil {
		return cost, cpu, true, err
	}
	if inv.crashed {
		return cost, cpu, true, errors.New(crashReason(n, inv.res))
	}
	return cost, cpu, false, nil
}

// abandon gives back the tests of ids the lost worker never finished: it returns them, and takes back the attempt
// number of every test that had begun, so the rerun is attempt 0 again and not a retry.
func (i *ingest) abandon(ids []shard.TestID) []shard.TestID {
	var left []shard.TestID
	for _, id := range ids {
		if !i.reported[string(id)] {
			left = append(left, id)
		}
	}
	i.ex.mu.Lock()
	for test := range i.cur {
		if i.ex.attempts[test] > 0 {
			i.ex.attempts[test]--
		}
	}
	i.ex.mu.Unlock()
	return left
}

func idStrings(ids []shard.TestID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func joinIDs(ids []shard.TestID) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(string(id) + "\n")
	}
	return b.String()
}

// shardSink writes a worker's shard output into this machine's log directory. It refuses anything after the
// assignment ended, so a worker that was given up on cannot write into a requeued shard.
type shardSink struct {
	reportFile, logFile, outDir string
	closed                      atomic.Bool
	mu                          sync.Mutex
}

func (s *shardSink) close() { s.mu.Lock(); s.closed.Store(true); s.mu.Unlock() }

func (s *shardSink) appendTo(path string, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return errors.New("the shard assignment has ended")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(p)
	return err
}

func (s *shardSink) Report(p []byte) error { return s.appendTo(s.reportFile, p) }
func (s *shardSink) Log(p []byte) error    { return s.appendTo(s.logFile, p) }

// Artifact limits: a worker is another machine, so what it sends is untrusted.
const (
	MaxArtifactBytes   = 512 << 20
	maxArtifactEntries = 20000
)

func (s *shardSink) Artifacts(r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return errors.New("the shard assignment has ended")
	}
	return UntarConfined(r, s.outDir, MaxArtifactBytes, maxArtifactEntries)
}

// UntarConfined unpacks a tar into dir. Only regular files and directories are written, every path must stay inside
// dir, and the total size and entry count are capped.
func UntarConfined(r io.Reader, dir string, maxBytes int64, maxEntries int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if n >= maxEntries {
			return errors.New("artifact archive has too many entries")
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("artifact entry %q leaves the directory", h.Name)
		}
		dst := filepath.Join(root, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if h.Size < 0 || total+h.Size > maxBytes {
				return errors.New("artifact archive is too large")
			}
			total += h.Size
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("artifact entry %q is not a plain file or directory", h.Name)
		}
	}
}

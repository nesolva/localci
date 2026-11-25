package suite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
)

// shardEnv is what one shard owns while it runs: a database cloned from the template, free ports and the
// stack's processes. Shard 0 is the serial group's (and the reruns').
type shardEnv struct {
	ex     *execution
	n      int
	dbName string
	dbURL  string
	shared bool // dbName is the run's shared database: the run owns it, close() leaves it

	mu       sync.Mutex
	ports    map[string]int
	portList []int
	out      string // $LOCALCI_OUTPUT_DIR of the invocation in progress
	svcs     []*exec.Service
	rssKB    int64   // sum of the peaks of every process that ran
	cpuPct   float64 // same, percent of one core
	once     sync.Once
}

// openEnv clones the database and starts the stack, ready to take the tests in ids.
func (ex *execution) openEnv(ctx context.Context, n int, ids []shard.TestID) (*shardEnv, error) {
	if e, ok := ex.borrowStack(n, ids); ok {
		return e, nil
	}
	e := &shardEnv{ex: ex, n: n, ports: map[string]int{}}
	ex.mu.Lock()
	for _, id := range ids {
		ex.envOf[string(id)] = e
	}
	ex.mu.Unlock()

	if ex.db != nil {
		// dbName is set only once the clone is ours: close() drops it, and a clone that failed because the
		// name exists means the database belongs to someone else.
		name := shard.ShardName(ex.instance(), ex.suite.Name, ex.id, n)
		if ex.iso != nil && ex.iso.shared != "" && n >= 1 && n <= ex.iso.nShared {
			name, e.shared = ex.iso.shared, true
		} else if err := ex.db.drv.Clone(ctx, ex.db.tpl, name); err != nil {
			e.close()
			return nil, fmt.Errorf("clone database: %w", err)
		} else if err := ex.checkLogSchema(ctx, name); err != nil {
			e.dbName = name // ours: close() drops it
			e.close()
			return nil, err
		}
		e.dbName = name
		url, err := ex.db.urlFor(e.dbName)
		if err != nil {
			e.close()
			return nil, err
		}
		e.dbURL = url
	}
	for _, sv := range ex.suite.Stack {
		if err := e.startService(ctx, sv); err != nil {
			e.close()
			return nil, fmt.Errorf("stack %s: %w", sv.Name, err)
		}
	}
	return e, nil
}

func (e *shardEnv) templ() env {
	return env{shard: e.n, run: e.ex.id, dbName: e.dbName, dbURL: e.dbURL,
		port: func(name string) (int, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if p, ok := e.ports[name]; ok {
				return p, nil
			}
			ps, err := exec.FreePorts(1)
			if err != nil {
				return 0, err
			}
			e.ports[name] = ps[0]
			e.portList = append(e.portList, ps[0])
			return ps[0], nil
		},
		dir: e.ex.dir,
	}
}

// dir resolves {{dir "repo"}}: where the run checked the repository out.
func (ex *execution) dir(repo string) (string, error) {
	if d, ok := ex.dirs[repo]; ok {
		return d, nil
	}
	return "", fmt.Errorf("{{dir %q}}: repository is not in the suite", repo)
}

// coreEnv is what every process of the shard gets; it wins over the suite's own env.
func (e *shardEnv) coreEnv() []string {
	out := append([]string{"LOCALCI_SHARD=" + fmt.Sprint(e.n), "LOCALCI_RUN=" + e.ex.id}, e.tenantEnv()...)
	if e.dbURL != "" {
		out = append(out, "LOCALCI_DB_URL="+e.dbURL, "LOCALCI_DB_NAME="+e.dbName)
	}
	return out
}

func (e *shardEnv) userEnv(m map[string]string) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var out []string
	for _, k := range keys {
		v, err := e.templ().expand(m[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

func (e *shardEnv) startService(ctx context.Context, sv Service) error {
	t := e.templ()
	cmd, err := t.expand(sv.Run)
	if err != nil {
		return err
	}
	user, err := e.userEnv(sv.Env)
	if err != nil {
		return err
	}
	ready := exec.Ready{Log: sv.Ready.Log, Timeout: sv.Ready.Timeout}
	if sv.Ready.HTTP != "" {
		if ready.HTTP, err = t.expand(sv.Ready.HTTP); err != nil {
			return err
		}
	}
	if sv.Ready.TCP != "" {
		s, err := t.expand(sv.Ready.TCP)
		if err != nil {
			return err
		}
		if _, err := fmt.Sscan(s, &ready.TCP); err != nil {
			return fmt.Errorf("ready.tcp %q is not a port", s)
		}
	}
	spec := exec.Spec{Cmd: cmd, Dir: e.ex.dirs[sv.Repo], Env: append(user, e.coreEnv()...),
		LogPath:   filepath.Join(e.ex.logDir, fmt.Sprintf("shard-%d.%s.log", e.n, slug(sv.Name))),
		KillGrace: 5 * time.Second, SampleEvery: time.Second}
	svc, err := exec.StartService(ctx, spec, ready)
	if err != nil {
		if e.ex.ctx.Err() == nil { // a run that was cancelled while it waited did not meet a broken stack
			why := err.Error()
			var se *exec.ServiceError
			if errors.As(err, &se) {
				why = se.Reason // without the end of its log, which is in the shard's log file
			}
			e.ex.setInfra(fmt.Sprintf("stack %s did not start: %s", sv.Name, why))
		}
		return err
	}
	e.mu.Lock()
	e.svcs = append(e.svcs, svc)
	e.mu.Unlock()
	svc.OnExit(func(r exec.Result) { e.serviceDied(sv.Name, r) })
	return nil
}

// serviceDied is the stack losing a service before anyone stopped it. Tests that run on from here prove nothing
// (and a rerun on a dead stack proves less), so the run is an infrastructure failure and ends now: the processes
// of the test commands still running are stopped and their tests end interrupted.
func (e *shardEnv) serviceDied(name string, r exec.Result) {
	ex := e.ex
	if ex.ctx.Err() != nil { // the run is ending anyway: the context took the service down
		return
	}
	ex.setInfra(fmt.Sprintf("stack %s exited before the run ended (code %d)", name, r.ExitCode))
	ex.cancel(errInfra)
}

// close stops the stack, drops the database and frees the ports. It is safe to call twice.
func (e *shardEnv) close() {
	e.once.Do(func() {
		for i := len(e.svcs) - 1; i >= 0; i-- {
			e.measure(e.svcs[i].Stop())
		}
		if e.dbName != "" && !e.shared {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ex.ctx), 30*time.Second)
			if err := e.ex.db.drv.Drop(ctx, e.dbName); err != nil {
				e.ex.emit(e.n, "db.drop.failed", "", "", map[string]string{"db": e.dbName, "error": err.Error()})
			}
			cancel()
		}
		exec.ReleasePorts(e.portList...)
		e.ex.mu.Lock()
		for id, env := range e.ex.envOf {
			if env == e {
				delete(e.ex.envOf, id)
			}
		}
		e.ex.mu.Unlock()
	})
}

func (e *shardEnv) measure(r exec.Result) {
	e.mu.Lock()
	e.rssKB += r.PeakRSSKB
	e.cpuPct += r.PeakCPU
	e.mu.Unlock()
}

// cost is what the shard was seen to use.
func (e *shardEnv) cost() (sched.Cost, float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return sched.Cost{Memory: uint64(e.rssKB) * 1024}, e.cpuPct / 100
}

type invocation struct {
	res     exec.Result
	crashed bool // the test command ended without finishing its report
}

// invokeSpec is the test command of one invocation, ready to start, and where its report and artifacts go.
type invokeSpec struct {
	spec       exec.Spec
	reportFile string
	outDir     string
}

// buildInvoke writes the list of tests ids and makes the command that runs them. tag distinguishes the files of the
// reruns of one shard.
func (e *shardEnv) buildInvoke(ids []shard.TestID, tag string) (invokeSpec, error) {
	ex := e.ex
	base := fmt.Sprintf("shard-%d%s", e.n, tag)
	testsFile := filepath.Join(ex.logDir, base+".tests")
	reportFile := filepath.Join(ex.logDir, base+".ndjson")
	var list strings.Builder
	for _, id := range ids {
		list.WriteString(string(id) + "\n")
	}
	if err := os.WriteFile(testsFile, []byte(list.String()), 0o644); err != nil {
		return invokeSpec{}, err
	}
	t := e.templ()
	cmd, err := t.expand(ex.suite.Test.Run)
	if err != nil {
		return invokeSpec{}, err
	}
	user, err := e.userEnv(ex.suite.Test.Env)
	if err != nil {
		return invokeSpec{}, err
	}
	// The framework's artifacts (screenshots, traces, videos) go next to the logs, not into the workspace: the
	// next checkout cleans the workspace and a rerun of the same shard would empty a shared directory.
	outDir := filepath.Join(ex.logDir, base+".out")
	e.mu.Lock()
	e.out = outDir
	e.mu.Unlock()
	core := append(e.coreEnv(), "LOCALCI_REPORT="+reportFile, "LOCALCI_TESTS="+testsFile, "LOCALCI_OUTPUT_DIR="+outDir)
	if ex.r.d.API != "" {
		core = append(core, "LOCALCI_API="+strings.TrimRight(ex.r.d.API, "/")+"/runs/"+ex.id, "LOCALCI_HOOK_TOKEN="+ex.hookToken)
	}
	spec := exec.Spec{Cmd: cmd, Dir: ex.dirs[ex.suite.Test.Repo], Env: append(user, core...),
		LogPath: filepath.Join(ex.logDir, base+".log"), KillGrace: 5 * time.Second, SampleEvery: time.Second}
	return invokeSpec{spec: spec, reportFile: reportFile, outDir: outDir}, nil
}

// invoke runs the suite's test command once for ids and ingests its report. tag distinguishes the files of
// the reruns of one shard.
func (e *shardEnv) invoke(ctx context.Context, ids []shard.TestID, m mode, tag string) (invocation, error) {
	ex := e.ex
	is, err := e.buildInvoke(ids, tag)
	if err != nil {
		return invocation{}, err
	}
	spec, reportFile := is.spec, is.reportFile
	ing := newIngest(ex, e.n, m)
	started := time.Now()
	stopTail, tailDone := ing.follow(reportFile)
	p, err := exec.Start(ctx, spec)
	if err != nil {
		stopTail()
		<-tailDone
		return invocation{}, err
	}
	res := p.Wait()
	stopTail()
	<-tailDone
	e.measure(res)
	crashed := ing.finish(res, ids) // tests that never reported end as interrupted, even when stopped
	if ctx.Err() != nil {
		return invocation{res: res}, ctx.Err()
	}
	if ing.versionErr != nil {
		return invocation{res: res, crashed: crashed}, ing.versionErr
	}
	if err := e.settleTenant(m, ids, started); err != nil {
		return invocation{res: res}, err
	}
	return invocation{res: res, crashed: crashed}, nil
}

func crashReason(n int, res exec.Result) string {
	if res.ExitCode == 0 {
		return fmt.Sprintf("shard %d: the test command wrote no complete report (is the reporter installed?)", n)
	}
	return fmt.Sprintf("shard %d: the test command exited with code %d before finishing", n, res.ExitCode)
}

// runShard runs one parallel shard from start to teardown.
func (ex *execution) runShard(n int, ids []shard.TestID) error {
	log := filepath.Join(ex.logDir, fmt.Sprintf("shard-%d.log", n))
	ex.putShard(n, store.StateRunning, log, sched.Cost{}, 0)
	e, err := ex.openEnv(ex.ctx, n, ids)
	if err != nil {
		ex.putShard(n, store.StateFailed, log, sched.Cost{}, 0)
		return fmt.Errorf("shard %d: %w", n, err)
	}
	defer e.close()
	inv, err := e.invoke(ex.ctx, ids, modeParallel, "")
	e.close()
	cost, cpu := e.cost()
	state := store.StatePassed
	if err != nil || inv.crashed || ex.laneFailed(n) {
		state = store.StateFailed
	}
	ex.putShard(n, state, log, cost, cpu)
	if err != nil {
		return err
	}
	if inv.crashed {
		return errors.New(crashReason(n, inv.res))
	}
	return nil
}

// runAlone runs the serial group, then reruns each test that failed in parallel alone, so that conflicts show.
func (ex *execution) runAlone() error {
	serial := ex.layout.Serial
	failed := ex.failedParallel()
	probes := ex.probes(failed)
	if len(serial) == 0 && len(failed) == 0 && len(probes) == 0 {
		return nil
	}
	log := filepath.Join(ex.logDir, "shard-0.log")
	all := append(append(slices.Clone(serial), failed...), probes...)
	ex.putShard(0, store.StateRunning, log, sched.Cost{}, 0)
	e, err := ex.openEnv(ex.ctx, 0, all)
	if err != nil {
		ex.putShard(0, store.StateFailed, log, sched.Cost{}, 0)
		return fmt.Errorf("serial group: %w", err)
	}
	defer e.close()

	var runErr error
	if len(serial) > 0 {
		inv, err := e.invoke(ex.ctx, serial, modeSerial, "")
		switch {
		case err != nil:
			runErr = err
		case inv.crashed:
			runErr = errors.New(crashReason(0, inv.res))
		}
	}
	if runErr == nil && len(failed) > 0 {
		inv, err := e.invoke(ex.ctx, failed, modeRerun, ".rerun1")
		if err != nil {
			runErr = err
		} else if again := ex.passedAlone(failed); len(again) > 0 && !inv.crashed {
			if _, err := e.invoke(ex.ctx, again, modeRerun, ".rerun2"); err != nil {
				runErr = err
			}
		}
	}
	if runErr == nil && len(failed) > 0 {
		runErr = ex.extraReruns(e, failed)
	}
	if runErr == nil && len(probes) > 0 {
		// Suspects of a shared database run alone only to see what they write; the run never counts as a result.
		if _, err := e.invoke(ex.ctx, probes, modeProbe, ".probe"); err != nil {
			runErr = err
		}
	}
	e.close()
	cost, cpu := e.cost()
	state := store.StatePassed
	if runErr != nil {
		state = store.StateFailed
	}
	ex.putShard(0, state, log, cost, cpu)
	return runErr
}

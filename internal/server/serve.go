package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/nesolva/localci/internal/affected"
	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/explore"
	"github.com/nesolva/localci/internal/flow"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/notify"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Options start a whole localci: the server, the runners and the pull request watcher.
type Options struct {
	Config  *config.Effective
	Reload  func() (*config.Effective, error) // what the Settings page and /api/config read
	DataDir string
	Machine capacity.Machine
	Log     *log.Logger
	// Ready is called with the TCP address once the server listens (the API socket is listening by then:
	// FindSocket(DataDir) says where).
	Ready func(addr string)
	// Port overrides server.port; -1 means "any free port" (tests).
	Port int
	// WorkerPort overrides server.worker_port the same way; WorkerReady is called with the worker listener's address.
	WorkerPort  int
	WorkerReady func(addr string)
	// Forge replaces GitHub; nil uses GitHub when a token is found.
	Forge forge.Forge
	// Notifier shows a desktop notification when a run started by hand ends in a `notify.on` state; nil: none.
	Notifier notify.Sender
}

// Start serves until ctx is done: runs suites and checks, watches the configured repositories, and answers the API
// and the dashboard on 127.0.0.1. Runs that a previous process left unfinished are cancelled first.
func Start(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = log.New(os.Stderr, "", log.LstdFlags)
	}
	cfg := &o.Config.Config
	if err := store.Harden(o.DataDir); err != nil {
		return fmt.Errorf("data directory %s: %w", o.DataDir, err)
	}
	st, err := store.Open(filepath.Join(o.DataDir, "localci.db"))
	if err != nil {
		return fmt.Errorf("open the state database: %w", err)
	}
	defer st.Close()
	token, err := LoadToken(o.DataDir)
	if err != nil {
		return fmt.Errorf("write token: %w", err)
	}

	port := cfg.Server.Port
	if o.Port != 0 {
		port = max(o.Port, 0)
	}
	l, err := Listen(port)
	if err != nil {
		return fmt.Errorf("listen on 127.0.0.1:%d: %w (is another `localci serve` running? change server.port)", port, err)
	}
	defer l.Close()
	sl, sockPath, err := ListenSocket(o.DataDir)
	if err != nil {
		l.Close()
		return fmt.Errorf("api socket: %w", err)
	}
	defer RemovePointer(o.DataDir)
	defer sl.Close()
	base := "http://" + l.Addr().String()

	bus := event.NewBus()
	elog := event.NewLog(st, bus)
	sc := sched.New(cfg.Capacity.CPUs, cfg.Capacity.Memory).WithDockerMemory(cfg.Capacity.DockerMemory)

	suites, err := suite.FromConfig(cfg.Suites)
	if err != nil {
		l.Close()
		return fmt.Errorf("suites: %w", err)
	}
	hub := NewWorkerHub(0)
	runner, err := suite.New(suite.Deps{Store: st, Log: elog, Sched: sc, DataDir: o.DataDir, Suites: suites, API: base,
		Remote: hub, RemoteForks: cfg.Workers.Forks, RepoURL: localRepoURL(cfg.Repos)})
	if err != nil {
		l.Close()
		return err
	}
	defer runner.Close()
	flow.Attach(runner, &flow.Service{Store: st, DataDir: o.DataDir})
	if err := (&affected.Planner{Store: st}).Register(runner); err != nil {
		l.Close()
		return err
	}

	url := func(run string) string { return base + "/runs/" + run }
	cr := &checks.Runner{
		Options: checks.Options{DataDir: o.DataDir, MergeRef: cfg.Fidelity.MergeRef, PathFilters: cfg.Fidelity.PathFilters,
			Toolchains: cfg.Fidelity.Toolchains, DockerCache: cfg.Fidelity.DockerCache, URL: url,
			SkipWorkflows: map[string][]string{}},
		Store: st, Admit: checkAdmitter{sc}, Log: elog, Context: ctx, Act: checks.RunAct,
	}
	for _, r := range cfg.Repos {
		cr.Options.SkipWorkflows[r.Name] = r.SkipWorkflows
	}

	var fg forge.Forge = o.Forge
	if fg == nil && len(cfg.Repos) > 0 {
		if tok, err := forge.ResolveToken(); err != nil {
			o.Log.Printf("pull requests are not watched: %v", err)
		} else {
			gh := forge.NewGitHub(withErrors(forge.OptionsFromConfig(cfg, tok), o.Log))
			fg = gh
		}
	}
	if fg != nil {
		cr.Forge = fg
	}

	if err := cancelLeftovers(ctx, st, elog, fg, o.Log); err != nil {
		l.Close()
		return err
	}

	go sweepLeftovers(ctx, st, suites, o.DataDir, o.Log)

	var d *Dispatcher
	if fg != nil && len(cfg.Repos) > 0 {
		d = &Dispatcher{Forge: fg, Suites: runner, Checks: cr, Store: st, Bus: bus,
			Config: func() *config.Config { return cfg }, URL: url, Log: o.Log}
	}
	var ex *explore.Service
	if ecfg, err := explore.Parse(cfg.Explore); err != nil {
		l.Close()
		return err
	} else if ecfg != nil {
		if ex, err = explore.New(*ecfg, runner, st); err != nil {
			l.Close()
			return err
		}
		ex.Log = o.Log.Printf
		go ex.Schedule(ctx)
	}
	var wl net.Listener
	var wcert *tls.Certificate
	var wfp string
	wport := cfg.Server.WorkerPort
	if o.WorkerPort != 0 {
		wport = max(o.WorkerPort, 0)
	}
	if c, fp, err := LoadWorkerCert(o.DataDir, o.Log.Printf); err != nil {
		o.Log.Printf("workers are off: certificate: %v", err)
	} else if wl, err = ListenWorker(cfg.Server.Listen, wport); err != nil {
		wl = nil
		o.Log.Printf("workers are off: listen on port %d: %v (change server.worker_port)", wport, err)
	} else {
		wcert, wfp = &c, fp
		defer wl.Close()
	}
	deps := Deps{Explore: ex, ExploreCtx: ctx, Store: st, Bus: bus, Suites: runner, Checks: cr, Sched: sc, Config: o.Reload,
		Machine: o.Machine, DataDir: o.DataDir, Token: token, Socket: sockPath,
		WorkerCert: wcert, WorkerFingerprint: wfp, WorkerListen: cfg.Server.Listen, Workers: hub}
	for _, su := range suites {
		deps.GitRepos = append(deps.GitRepos, su.Repos...)
	}
	if wl != nil {
		deps.WorkerPort = wl.Addr().(*net.TCPAddr).Port
	}
	if d != nil {
		deps.Report = func(run, name string, heads []forge.Head, since int64) { go d.report(ctx, run, name, heads, since) }
	}
	srv := New(deps)
	if o.Notifier != nil {
		go notify.Watch(ctx, st, bus, cfg.Notify.On, o.Notifier, url, o.Log.Printf)
	}

	if d != nil {
		go func() {
			if err := fg.Watch(ctx, func(t forge.Trigger) error { return d.Handle(ctx, t) }); err != nil && ctx.Err() == nil {
				o.Log.Printf("watching pull requests stopped: %v", err)
			}
		}()
	}

	if o.Ready != nil {
		o.Ready(l.Addr().String())
	}
	o.Log.Printf("localci serving the dashboard at %s and the API on %s", base, sockPath)
	sctx, stopSocket := context.WithCancel(ctx)
	sockDone := make(chan error, 1)
	go func() { sockDone <- srv.ServeSocket(sctx, sl) }()
	workDone := make(chan error, 1)
	if wl != nil {
		o.Log.Printf("workers join at port %d (%s)", deps.WorkerPort, cfg.Server.Listen)
		if o.WorkerReady != nil {
			o.WorkerReady(wl.Addr().String())
		}
		go func() { workDone <- srv.ServeWorkers(sctx, wl) }()
	} else {
		workDone <- nil
	}
	err = srv.Serve(ctx, l)
	stopSocket()
	if serr := <-sockDone; err == nil {
		err = serr
	}
	<-workDone
	cr.Wait()
	return err
}

func withErrors(o forge.Options, lg *log.Logger) forge.Options {
	o.OnError = func(err error) { lg.Printf("github: %v", err) }
	return o
}

// restartReason is why a run a previous process left unfinished ends, on the run and on its pull request.
const restartReason = "localci restarted"

// cancelLeftovers marks runs a previous process never finished as cancelled, so nothing shows as running that is
// not: it appends the run.state event (followers and the dashboard learn from events) and, for a run on a pull
// request, posts a final status so the pull request does not stay pending. fg may be nil. Pull requests are run
// again by their next push or label.
func cancelLeftovers(ctx context.Context, st *store.Store, elog *event.Log, fg forge.Forge, lg *log.Logger) error {
	runs, err := st.Runs(ctx, 0)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if store.Terminal(r.State) {
			continue
		}
		if state, reason, ok := loggedEnd(ctx, st, r.ID); ok {
			// The run ended and said so (its final status went out), only its row was not saved: keep that result.
			if err := st.SetRunState(ctx, r.ID, state, reason); err != nil {
				return err
			}
			continue
		}
		if err := st.SetRunState(ctx, r.ID, store.StateCancelled, restartReason); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]string{"state": store.StateCancelled, "reason": restartReason})
		if _, err := elog.Append(ctx, store.Event{Run: r.ID, Kind: "run.state", Data: data}); err != nil {
			return err
		}
		if fg == nil {
			continue
		}
		var heads []forge.Head
		if json.Unmarshal(r.Heads, &heads) != nil {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := forge.SetStatuses(sctx, fg, heads, forge.Status{Suite: suite.StatusName(r.Suite, r.Affected), State: forge.StateError, Description: restartReason})
		cancel()
		if err != nil {
			lg.Printf("status of run %s: %v", r.ID, err)
		}
	}
	return nil
}

// loggedEnd returns the terminal state a run's event log records, if any.
func loggedEnd(ctx context.Context, st *store.Store, run string) (state, reason string, ok bool) {
	evs, err := st.RunEvents(ctx, run)
	if err != nil {
		return "", "", false
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != "run.state" {
			continue
		}
		var d struct{ State, Reason string }
		if json.Unmarshal(evs[i].Data, &d) == nil && store.Terminal(d.State) {
			return d.State, d.Reason, true
		}
	}
	return "", "", false
}

// checkAdmitter lets checks wait in the same line as suites.
type checkAdmitter struct{ s *sched.Scheduler }

func (a checkAdmitter) Admit(ctx context.Context, c checks.Cost) (func(), error) {
	return a.s.Admit(ctx, sched.Cost{CPUs: int(math.Ceil(c.CPU)), Memory: uint64(max(c.Mem, 0))})
}

// localRepoURL says where suites clone a repository from: the checkout it is mapped to by `repos[].path`, else
// the default (its GitHub address, nil here). A repository that lives only on this machine has no other source.
func localRepoURL(repos []config.Repo) func(string) string {
	paths := map[string]string{}
	for _, r := range repos {
		if r.Path != "" {
			paths[r.Name] = r.Path
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return func(repo string) string {
		if p, ok := paths[repo]; ok {
			return p
		}
		return "https://github.com/" + repo + ".git"
	}
}

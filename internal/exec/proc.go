package exec

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Spec describes a command to run.
type Spec struct {
	Cmd string   // run as `bash -eo pipefail -c`
	Dir string   // working directory
	Env []string // KEY=VALUE, added to this process's environment
	// MinimalEnv starts the process from a short allowlist of this process's environment (search path, home, locale,
	// temp dir, terminal, proxies, the model provider's variables) instead of all of it, so a command that is not
	// ours, such as the explore agent, does not inherit the daemon's secrets.
	MinimalEnv bool
	Timeout    time.Duration
	LogPath    string // output (stdout and stderr, interleaved) is written here
	// Stdin is a file the command reads as its standard input; empty means none (/dev/null).
	Stdin string
	// KillGrace is how long SIGTERM gets before SIGKILL (default 10s).
	KillGrace time.Duration
	// SampleEvery is the CPU and memory sampling period (default 2s).
	SampleEvery time.Duration
	// OnSample receives each sample, from the sampling goroutine.
	OnSample func(Sample)
}

// Result is how a process ended.
type Result struct {
	ExitCode  int // 128+signal when killed by a signal
	TimedOut  bool
	Cancelled bool
	Duration  time.Duration
	PeakRSSKB int64
	PeakCPU   float64
}

// OK reports a clean exit that was not cut short.
func (r Result) OK() bool { return r.ExitCode == 0 && !r.TimedOut && !r.Cancelled }

// Proc is a running command in its own process group.
type Proc struct {
	spec  Spec
	cmd   *osexec.Cmd
	pgid  int
	tree  remembered
	hub   *logHub
	grace time.Duration

	cancel context.CancelFunc
	done   chan struct{}
	res    Result
}

// Start launches spec. The process and everything it spawns end when ctx is
// cancelled, the timeout passes or Cancel is called, and never outlive Wait.
func Start(ctx context.Context, spec Spec) (*Proc, error) {
	if spec.LogPath == "" {
		return nil, fmt.Errorf("exec: LogPath is required")
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o755); err != nil {
		return nil, err
	}
	hub, err := newLogHub(spec.LogPath)
	if err != nil {
		return nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		hub.finish()
		return nil, err
	}
	cmd := osexec.Command("bash", "-eo", "pipefail", "-c", spec.Cmd)
	cmd.Dir = spec.Dir
	base := os.Environ()
	if spec.MinimalEnv {
		base = minimalEnv(base)
	}
	cmd.Env = append(base, spec.Env...)
	cmd.Stdout, cmd.Stderr = pw, pw
	var in *os.File
	if spec.Stdin != "" {
		if in, err = os.Open(spec.Stdin); err != nil {
			pr.Close()
			pw.Close()
			hub.finish()
			return nil, err
		}
		cmd.Stdin = in
	}
	setGroup(cmd)
	err = cmd.Start()
	if in != nil {
		in.Close()
	}
	if err != nil {
		pr.Close()
		pw.Close()
		hub.finish()
		return nil, err
	}
	pw.Close()

	p := &Proc{spec: spec, cmd: cmd, pgid: cmd.Process.Pid, hub: hub, grace: spec.KillGrace, done: make(chan struct{})}
	if p.grace <= 0 {
		p.grace = 10 * time.Second
	}
	pctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	tctx := pctx
	if spec.Timeout > 0 {
		var c context.CancelFunc
		tctx, c = context.WithTimeout(pctx, spec.Timeout)
		prev := cancel
		p.cancel = func() { c(); prev() }
	}
	go p.run(pctx, tctx, pr, time.Now())
	return p, nil
}

// Run starts spec and waits for it.
func Run(ctx context.Context, spec Spec) (Result, error) {
	p, err := Start(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	return p.Wait(), nil
}

func (p *Proc) run(pctx, tctx context.Context, pr *os.File, began time.Time) {
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				p.hub.write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	sampStop, sampDone := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var peakRSS int64
	var peakCPU float64
	go func() {
		defer close(sampDone)
		every := p.spec.SampleEvery
		if every <= 0 {
			every = 2 * time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-sampStop:
				return
			case <-t.C:
				rows, ok := listProcs(tctx)
				if !ok {
					continue
				}
				p.tree.add(rows, p.pgid)
				s := treeStats(rows, p.pgid)
				s.At = time.Now()
				if s.Procs == 0 {
					continue
				}
				mu.Lock()
				peakRSS = max(peakRSS, s.RSSKB)
				peakCPU = max(peakCPU, s.CPU)
				mu.Unlock()
				if p.spec.OnSample != nil {
					p.spec.OnSample(s)
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- p.cmd.Wait() }()

	var res Result
	select {
	case <-waitCh:
	case <-tctx.Done():
		res.Cancelled = pctx.Err() != nil
		res.TimedOut = !res.Cancelled
		if rows, ok := listProcs(context.Background()); ok {
			p.tree.add(rows, p.pgid)
		}
		stopGroup(p.pgid, p.grace)
		<-waitCh
	}
	// Whatever the command left running goes too: background jobs in its group, and descendants that put
	// themselves in a group of their own.
	stopGroup(p.pgid, p.grace)
	p.tree.stop(p.grace)

	close(sampStop)
	<-sampDone
	// Writers are gone, so the pipe reaches EOF; a stray process that escaped
	// the group must not hold the log open for ever.
	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		pr.Close()
		<-pumpDone
	}
	pr.Close()
	p.hub.finish()

	res.ExitCode = exitCode(p.cmd.ProcessState)
	res.Duration = time.Since(began)
	mu.Lock()
	res.PeakRSSKB, res.PeakCPU = peakRSS, peakCPU
	mu.Unlock()
	p.res = res
	p.cancel()
	close(p.done)
}

// PID is the process id of the command (also its process group id).
func (p *Proc) PID() int { return p.pgid }

// LogPath is the file the output is written to.
func (p *Proc) LogPath() string { return p.hub.path }

// Done is closed when the process and its group are gone and the log is complete.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Wait blocks until the process is done.
func (p *Proc) Wait() Result {
	<-p.done
	return p.res
}

// Cancel stops the process group: SIGTERM, then SIGKILL after the grace period.
func (p *Proc) Cancel() { p.cancel() }

// Subscribe streams the output line by line from its start; the channel closes
// when the process is done or ctx ends.
func (p *Proc) Subscribe(ctx context.Context) <-chan string { return p.hub.subscribe(ctx) }

// LastLines returns up to n trailing lines of the output so far.
func (p *Proc) LastLines(n int) []string { return lastLines(p.hub.path, n) }

// minimalEnv keeps the variables a tool needs to run and to reach its network (proxies, CA bundles) and provider,
// and drops the rest.
func minimalEnv(environ []string) []string {
	exact := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LANG": true, "TMPDIR": true, "TERM": true,
		"SHELL": true, "LOGNAME": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true}
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if exact[k] || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_") || strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") {
			out = append(out, kv)
		}
	}
	return out
}

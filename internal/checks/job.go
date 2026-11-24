package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/workflow"
)

const (
	defaultImage   = "ubuntu:24.04"
	healthTimeout  = 2 * time.Minute
	teardownBudget = time.Minute
)

// job is one check being run.
type job struct {
	r      *Runner
	req    Request
	check  workflow.Check
	mirror *exec.Mirror
	sha    string // the commit under test (merge result, or snapshot)
	tree   string
	id     string
	name   string // `<workflow>/<job>`, the status context without its prefix
	key    string // supersede key, empty without a pull request
	cancel context.CancelCauseFunc
	done   chan struct{}

	rec     store.Run
	head    headInfo
	logDir  string
	logFile *os.File
	queued  bool // still counted in Runner.waiting
}

type outcome struct {
	state, reason string
	status        forge.Status
}

func (j *job) url() string {
	if j.r.Options.URL == nil {
		return ""
	}
	return j.r.Options.URL(j.id)
}

func (j *job) shardLog() string { return filepath.Join(j.logDir, "run.log") }

// create records the run as queued and says so on the pull request.
func (j *job) create(ctx context.Context) error {
	shaForStatus := j.req.Head
	if j.req.Snapshot != "" {
		shaForStatus = j.req.Snapshot
	}
	j.head = headInfo{
		Head: forge.Head{Repo: j.req.Repo, Branch: j.req.Base, SHA: shaForStatus, PR: j.req.PR},
		Tree: j.tree,
	}
	if j.sha != shaForStatus {
		j.head.Merge = j.sha
	}
	j.logDir = filepath.Join(j.r.Options.DataDir, "logs", "checks", j.id)
	j.rec = store.Run{ID: j.id, Suite: j.name, Kind: store.KindCheck, Heads: j.head.json(),
		Trigger: j.req.Trigger, RequestedBy: j.req.RequestedBy, State: store.StateQueued, Created: time.Now().UnixMilli()}
	if err := j.r.Store.PutRun(ctx, j.rec); err != nil {
		return err
	}
	if err := j.r.Store.PutShard(ctx, store.Shard{Run: j.id, N: 1, State: store.StateQueued, LogPath: j.shardLog()}); err != nil {
		return err
	}
	j.emit(ctx, 0, "run.state", "", map[string]any{"state": store.StateQueued, "name": j.name})
	j.post(forge.Queued(j.name, int(j.r.waiting.Load()), j.url()))
	return nil
}

// run takes the check from queued to a terminal state.
func (j *job) run(ctx context.Context) {
	defer close(j.done)
	defer j.leaveQueue()
	var out outcome
	func() {
		defer func() {
			if p := recover(); p != nil {
				out = outcome{store.StateFailed, fmt.Sprintf("infrastructure: internal error: %v", p),
					j.errorStatus(fmt.Sprintf("internal error: %v", p))}
			}
		}()
		out = j.execute(ctx)
	}()
	if j.logFile != nil {
		j.logFile.Close()
	}
	j.finish(out)
}

func (j *job) leaveQueue() {
	if j.queued {
		j.queued = false
		j.r.waiting.Add(-1)
	}
}

func (j *job) finish(out outcome) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The status goes out even when the store is unwell: a pull request left
	// pending forever is worse than a run record that is behind. The run.state
	// event is appended either way: on restart it is what tells a run that
	// ended (and reported) from one that was cut short.
	if err := j.r.Store.SetRunState(ctx, j.id, out.state, out.reason); err != nil {
		j.logf("run state not saved: %v", err)
	} else {
		j.r.Store.PutShard(ctx, store.Shard{Run: j.id, N: 1, State: out.state, LogPath: j.shardLog()})
	}
	j.emit(ctx, 0, "run.state", "", map[string]any{"state": out.state, "reason": out.reason})
	j.post(out.status)
}

func (j *job) errorStatus(desc string) forge.Status {
	return forge.Status{Suite: j.name, State: forge.StateError, TargetURL: j.url(), Description: truncate(desc)}
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= forge.MaxDescription {
		return s
	}
	return string(r[:forge.MaxDescription-1]) + "…"
}

// post puts a status on the pull request head; a failure goes to the log, it
// never fails the check.
func (j *job) post(st forge.Status) {
	if j.req.PR == 0 || j.r.Forge == nil || st.Suite == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := forge.SetStatuses(ctx, j.r.Forge, []forge.Head{j.head.Head}, st); err != nil {
		j.logf("status not posted: %v", err)
	}
}

func (j *job) emit(ctx context.Context, shard int, kind, step string, data map[string]any) {
	if j.r.Log == nil {
		return
	}
	b, _ := json.Marshal(data)
	j.r.Log.Append(ctx, store.Event{TS: time.Now().UnixMilli(), Run: j.id, Shard: shard, Kind: kind, Step: step, Data: b})
}

func (j *job) setState(ctx context.Context, state string) {
	j.r.Store.SetRunState(ctx, j.id, state, "")
	j.r.Store.PutShard(ctx, store.Shard{Run: j.id, N: 1, State: state, LogPath: j.shardLog()})
	j.emit(ctx, 0, "run.state", "", map[string]any{"state": state})
}

func (j *job) logf(format string, args ...any) {
	if j.logFile != nil {
		fmt.Fprintf(j.logFile, format+"\n", args...)
	}
}

// stopped is the outcome of a run that was cancelled or superseded.
func (j *job) stopped(ctx context.Context) (outcome, bool) {
	if ctx.Err() == nil {
		return outcome{}, false
	}
	reason := "cancelled"
	if errors.Is(context.Cause(ctx), errSuperseded) {
		reason = "superseded"
	}
	return outcome{store.StateCancelled, reason, j.errorStatus("cancelled: " + reason)}, true
}

func (j *job) infra(err error) outcome {
	return outcome{store.StateFailed, "infrastructure: " + err.Error(), j.errorStatus(err.Error())}
}

func (j *job) execute(ctx context.Context) outcome {
	c := j.check
	if len(c.Steps) == 0 && c.Unsupported != "" {
		return outcome{store.StateFailed, "unsupported: " + c.Unsupported, j.errorStatus("unsupported: " + c.Unsupported)}
	}
	if err := os.MkdirAll(j.logDir, 0o755); err != nil {
		return j.infra(err)
	}
	var err error
	if j.logFile, err = os.Create(j.shardLog()); err != nil {
		return j.infra(err)
	}
	if c.Unsupported != "" {
		j.logf("warning: this check may differ from GitHub's: %s", c.Unsupported)
	}

	tc, err := exec.ResolveToolchain(ctx, func(p string) ([]byte, error) {
		b, err := gitBytes(ctx, j.mirror.Dir, "show", j.sha+":"+p)
		return b, err
	}, c.Toolchain, j.r.Options.Toolchains)
	if err != nil {
		return j.infra(err)
	}
	j.head.Toolchain = tc.Key()
	j.head.Reuse = reuseKey(c, j.tree, j.head.Toolchain)
	j.rec.Heads = j.head.json()
	if err := j.r.Store.PutRun(ctx, j.rec); err != nil {
		return j.infra(err)
	}

	if !j.req.Force {
		if prev, ok := j.r.findReusable(ctx, j.name, j.head.Reuse); ok {
			reason := "reused #" + prev.ID
			j.logf("%s: this tree already passed this check", reason)
			return outcome{store.StateReused, reason, forge.Status{Suite: j.name, State: forge.StateSuccess,
				TargetURL: j.url(), Description: reason}}
		}
	}

	unlockTags, err := j.r.tags.lock(ctx, c.DockerTags)
	if err != nil {
		o, _ := j.stopped(ctx)
		return o
	}
	defer unlockTags()
	cost := j.r.Options.Cost
	if cost == (Cost{}) {
		cost = Cost{CPU: 1, Mem: 1 << 30}
	}
	release, err := j.r.Admit.Admit(ctx, cost)
	j.leaveQueue()
	if err != nil {
		if o, ok := j.stopped(ctx); ok {
			return o
		}
		return j.infra(err)
	}
	defer release()

	ws := exec.OpenWorkspace(filepath.Join(j.r.Options.DataDir, "workspaces"), "check", j.req.Repo+"/"+j.name)
	lk, _ := j.r.wsLocks.LoadOrStore(ws.Dir, &sync.Mutex{})
	mu := lk.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	j.setState(ctx, store.StatePreparing)
	if err := ws.Checkout(ctx, j.mirror, j.sha, j.r.Options.Keep); err != nil {
		if o, ok := j.stopped(ctx); ok {
			return o
		}
		return j.infra(fmt.Errorf("checkout: %w", err))
	}
	if c.NeedsActions {
		return j.runAct(ctx, ws.Dir)
	}

	j.setState(ctx, store.StateRunning)
	started := time.Now()
	j.post(forge.Running(j.name, 0, len(c.Steps), started, j.url()))
	return j.runSteps(ctx, ws.Dir, tc, started)
}

func (j *job) runAct(ctx context.Context, dir string) outcome {
	started := time.Now()
	j.setState(ctx, store.StateRunning)
	j.post(forge.Running(j.name, 0, 1, started, j.url()))
	// act writes the run log itself, so it streams like any other check's.
	if j.logFile != nil {
		j.logFile.Close()
		j.logFile = nil
	}
	res, err := j.r.Act(ctx, ActJob{Check: j.check, Dir: dir, LogPath: j.shardLog(), Request: j.req, Merge: j.sha, Run: j.id})
	if o, ok := j.stopped(ctx); ok {
		return o
	}
	switch {
	case errors.Is(err, exec.ErrActRanNothing):
		// Exit 0 with no job run is a check that proved nothing, never a pass.
		return outcome{store.StateFailed, err.Error(), forge.Failed(j.name, 1, err.Error(), j.url())}
	case err != nil:
		return j.infra(err)
	case res.TimedOut:
		return outcome{store.StateFailed, "timed out after " + j.check.Timeout.String(),
			forge.Failed(j.name, 1, "act (timed out)", j.url())}
	case !res.OK():
		return outcome{store.StateFailed, "act job failed, see log (exit " + fmt.Sprint(res.ExitCode) + ")",
			forge.Failed(j.name, 1, "act job failed, see log", j.url())}
	}
	return outcome{store.StatePassed, "", forge.Passed(j.name, 1, time.Since(started), j.url())}
}

// stack is what a job's steps run in: nothing (the host), or a container with
// its services on a private network.
type stack struct {
	j          *job
	network    string
	containers []string
	jobCont    string
	ws         string
}

// down removes everything the stack started, whatever happened to the run.
func (s *stack) down() {
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	if s.jobCont != "" && runtime.GOOS == "linux" {
		// Files the container created in the workspace belong to root: give them back.
		exec.Chown(ctx, s.jobCont, os.Getuid(), os.Getgid(), s.ws)
	}
	for i := len(s.containers) - 1; i >= 0; i-- {
		if err := exec.RemoveContainer(ctx, s.containers[i]); err != nil {
			s.j.logf("cleanup: %v", err)
		}
	}
	if s.network != "" {
		if err := exec.RemoveNetwork(ctx, s.network); err != nil {
			s.j.logf("cleanup: %v", err)
		}
	}
}

// up starts the services and the job container the check needs, if any. The
// returned stack must be taken down even when err is set.
func (j *job) up(ctx context.Context, ws string, tc exec.Toolchain) (*stack, string, error) {
	c := j.check
	s := &stack{j: j, ws: ws}
	image := ""
	var cont workflow.Container
	switch {
	case c.Container != nil:
		cont = *c.Container
		image = cont.Image
		if len(c.Toolchain) > 0 {
			j.logf("note: the job container %s provides its own tools; the declared toolchain is not installed", image)
		}
	case tc.Image != "":
		image = tc.Image
	case len(c.Services) > 0:
		// Services are reached by alias on a Docker network, so the job has to be on it.
		if image = tc.ContainerImage(); image == "" {
			if image = j.r.Options.DefaultImage; image == "" {
				image = defaultImage
			}
			if len(c.Toolchain) > 0 {
				j.logf("note: several tools need mise on the host, but services need a container: running in %s", image)
			}
		}
	}
	if image == "" {
		return s, "", nil
	}
	if !exec.DockerAvailable() {
		return s, "", errors.New("this check cannot run: Docker is not running, and it needs it")
	}
	if len(c.Services) > 0 {
		s.network = "localci-" + j.id
		if err := exec.CreateNetwork(ctx, s.network); err != nil {
			return s, "", err
		}
	}
	for i, svc := range c.Services {
		name := fmt.Sprintf("localci-%s-svc%d", j.id, i+1)
		spec := exec.ContainerSpec{Name: name, Image: svc.Image, Network: s.network, Aliases: []string{svc.Name},
			Env: svc.Env, Volumes: svc.Volumes, Options: svc.Options, Run: j.id}
		s.containers = append(s.containers, name)
		if err := exec.StartContainer(ctx, spec); err != nil {
			return s, "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		if strings.Contains(svc.Options, "--health-cmd") {
			if err := exec.WaitHealthy(ctx, name, healthTimeout); err != nil {
				return s, "", err
			}
		}
	}
	name := "localci-" + j.id + "-job"
	spec := exec.ContainerSpec{Name: name, Image: image, Network: s.network, Env: cont.Env, Workdir: ws,
		Volumes: append([]string{ws + ":" + ws, filepath.Join(j.mirror.Dir, "objects") + ":" + filepath.Join(j.mirror.Dir, "objects") + ":ro"}, cont.Volumes...),
		Options: cont.Options, Run: j.id}
	s.containers = append(s.containers, name)
	s.jobCont = name
	if err := exec.StartJobContainer(ctx, spec); err != nil {
		return s, "", fmt.Errorf("job container: %w", err)
	}
	return s, name, nil
}

func (j *job) baseEnv(ws string) map[string]string {
	env := map[string]string{
		"CI": "true", "GITHUB_ACTIONS": "true", "GITHUB_WORKSPACE": ws, "GITHUB_REPOSITORY": j.req.Repo,
		"GITHUB_SHA": j.sha, "GITHUB_EVENT_NAME": "pull_request",
	}
	if j.check.Container != nil {
		for k, v := range j.check.Container.Env {
			env[k] = v
		}
	}
	for k, v := range j.check.Env {
		env[k] = v
	}
	return env
}

func (j *job) runSteps(ctx context.Context, ws string, tc exec.Toolchain, started time.Time) outcome {
	c := j.check
	stk, container, err := j.up(ctx, ws, tc)
	defer stk.down()
	if err != nil {
		if o, ok := j.stopped(ctx); ok {
			return o
		}
		return j.infra(err)
	}
	deadline := started.Add(c.Timeout)
	base := j.baseEnv(ws)
	passed, failedStep := 0, ""
	for i, st := range c.Steps {
		if failedStep != "" && !st.Always {
			continue
		}
		if o, ok := j.stopped(ctx); ok {
			return o
		}
		sid := fmt.Sprintf("s%d", i+1)
		ok, timedOut, err := j.runStep(ctx, sid, st, ws, container, tc, base, time.Until(deadline))
		if o, stop := j.stopped(ctx); stop {
			return o
		}
		switch {
		case err != nil:
			return j.infra(err)
		case ok:
			passed++
		case timedOut:
			j.logf("==> timed out after %s", c.Timeout)
			return outcome{store.StateFailed, "timed out after " + c.Timeout.String(),
				forge.Failed(j.name, 1, st.Name+" (timed out)", j.url())}
		case st.ContinueOnError:
			passed++
		default:
			if failedStep == "" {
				failedStep = st.Name
			}
		}
	}
	if failedStep != "" {
		return outcome{store.StateFailed, "step failed: " + failedStep, forge.Failed(j.name, 1, failedStep, j.url())}
	}
	return outcome{store.StatePassed, "", forge.Passed(j.name, passed, time.Since(started), j.url())}
}

// runStep runs one step and records it. A step that cannot even start (an
// unsupported shell, a missing directory) is an error, not a failure.
func (j *job) runStep(ctx context.Context, sid string, st workflow.Step, ws, container string, tc exec.Toolchain,
	base map[string]string, remaining time.Duration) (ok, timedOut bool, err error) {
	if remaining <= 0 {
		return false, true, nil
	}
	shell, err := shellOf(st.Shell)
	if err != nil {
		return false, false, fmt.Errorf("step %q: %w", st.Name, err)
	}
	dir, err := stepDir(ws, st.WorkingDirectory)
	if err != nil {
		return false, false, fmt.Errorf("step %q: %w", st.Name, err)
	}
	script := st.Run
	if j.r.Options.DockerCache && container == "" {
		script = exec.RewriteDockerRun(script)
	}
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	for k, v := range st.Env {
		env[k] = v
	}
	spec := exec.Spec{Dir: dir, Timeout: remaining, LogPath: filepath.Join(j.logDir, sid+".log")}
	if container != "" {
		spec.Cmd = exec.ExecLine(container, dir, shell, env, script)
		spec.Dir = ws
	} else {
		if shell == "sh" {
			script = "sh -e -c " + exec.ShellQuote(script)
		}
		spec.Cmd = tc.Wrap(script)
		for _, k := range sortedKeys(env) {
			spec.Env = append(spec.Env, k+"="+env[k])
		}
	}

	begin := time.Now()
	title := st.Name
	j.r.Store.PutStep(ctx, store.Step{Run: j.id, ID: sid, Title: title, Kind: "step", Start: begin.UnixMilli()})
	j.emit(ctx, 1, "step.begin", sid, map[string]any{"title": title})
	j.logf("==> %s", title)
	p, err := exec.Start(ctx, spec)
	if err != nil {
		return false, false, fmt.Errorf("step %q: %w", st.Name, err)
	}
	for line := range p.Subscribe(context.Background()) {
		j.logf("%s", line)
	}
	res := p.Wait()
	ms := time.Since(begin).Milliseconds()
	j.r.Store.EndStep(context.WithoutCancel(ctx), j.id, "", sid, ms, res.OK())
	j.emit(context.WithoutCancel(ctx), 1, "step.end", sid, map[string]any{"ms": ms, "ok": res.OK()})
	return res.OK(), res.TimedOut, nil
}

// shellOf reduces a step's `shell:` to bash, sh, or "" when the step names none.
func shellOf(s string) (string, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", nil
	}
	switch filepath.Base(f[0]) {
	case "bash":
		return "bash", nil
	case "sh":
		return "sh", nil
	}
	return "", fmt.Errorf("shell %q is not supported (use bash or sh)", s)
}

// stepDir resolves a step's working-directory below the workspace.
func stepDir(ws, wd string) (string, error) {
	if wd == "" || wd == "." {
		return ws, nil
	}
	if filepath.IsAbs(wd) {
		return filepath.Clean(wd), nil
	}
	dir := filepath.Join(ws, wd)
	if rel, err := filepath.Rel(ws, dir); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working-directory %q leaves the workspace", wd)
	}
	return dir, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := gitBytes(ctx, dir, args...)
	return strings.TrimSpace(string(b)), err
}

func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

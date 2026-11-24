package explore

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// fakeClock moves only when a test says so.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, waiter{c.now.Add(d), ch})
	return ch
}

// Advance moves time forward and fires every waiter that is now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	st  *store.Store
	env suite.ExploreEnv
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := t.TempDir()
	g := &rig{st: st}
	g.env = suite.ExploreEnv{Run: "7", Suite: "e2e", Dir: t.TempDir(), OutDir: filepath.Join(logs, "explore.out"),
		LogPath: filepath.Join(logs, "agent.log"), BaseURL: "http://127.0.0.1:4310", Emit: func(string, any) {}}
	return g
}

func (g *rig) explorer(command string, minutes int, clock Clock) *Explorer {
	return &Explorer{Cfg: Config{Goals: []string{"buy as a new customer"}, Agent: Agent{Command: command, Minutes: minutes}}, Store: g.st, Clock: clock}
}

const twoFindings = `
printf '%s\n' '{"goal":"buy as a new customer","severity":"high","title":"Paying with a card returns a 500","steps":["add an item","pay"],"screenshot":"pay.png","test":"test(\"pay\", async () => {})"}' >> "$LOCALCI_FINDINGS"
printf 'png' > "$LOCALCI_OUTPUT_DIR/pay.png"
printf '%s\n' '{"severity":"low","title":"Typo on the cart page","steps":"open the cart\nread the heading"}' >> "$LOCALCI_FINDINGS"
`

func TestFindingsFromTheAgentAreStoredListedAndKeepTheirDraftTests(t *testing.T) {
	g := newRig(t)
	note, err := g.explorer(twoFindings, 5, SystemClock).Run(context.Background(), g.env)
	if err != nil || note != "2 findings" {
		t.Fatalf("note %q, err %v", note, err)
	}
	rec := httptest.NewRecorder()
	APIHandler(g.st).ServeHTTP(rec, httptest.NewRequest("GET", "/api/findings?run=7", nil))
	var l List
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if l.Total != 2 || l.Findings[1].Title != "Paying with a card returns a 500" || l.Findings[1].Severity != "high" {
		t.Fatalf("listed: %+v", l)
	}
	pay, typo := l.Findings[1], l.Findings[0]
	if want := filepath.Join(g.env.OutDir, "pay.png"); !samePath(pay.Screenshot, want) {
		t.Errorf("screenshot = %q, want %q", pay.Screenshot, want)
	}
	if b, _ := os.ReadFile(pay.Test); !strings.Contains(string(b), `test("pay"`) || !strings.HasPrefix(pay.Test, g.env.OutDir) {
		t.Errorf("draft test %q: %q", pay.Test, b)
	}
	if len(typo.Steps) != 2 || typo.Test != "" || typo.Run != "7" || typo.Suite != "e2e" {
		t.Errorf("second finding: %+v", typo)
	}
	// The cursor only moves forward: nothing is listed after the newest finding.
	rec = httptest.NewRecorder()
	APIHandler(g.st).ServeHTTP(rec, httptest.NewRequest("GET", "/api/findings?since="+itoa(l.Cursor), nil))
	var after List
	json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Total != 0 {
		t.Errorf("after the cursor: %d findings", after.Total)
	}
}

func TestTimeLimitKillsTheAgentAndKeepsWhatItWrote(t *testing.T) {
	g := newRig(t)
	clock := newClock(time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC))
	// Reports one finding, records its pid, then goes on as an agent that never finishes would.
	cmd := `printf '%s\n' '{"severity":"medium","title":"Found before the limit"}' >> "$LOCALCI_FINDINGS"; echo $$ > "$LOCALCI_OUTPUT_DIR/pid"; exec tail -f /dev/null`
	type out struct {
		note string
		err  error
	}
	done := make(chan out, 1)
	go func() {
		n, err := g.explorer(cmd, 30, clock).Run(context.Background(), g.env)
		done <- out{n, err}
	}()
	pidFile := filepath.Join(g.env.OutDir, "pid")
	eventually(t, "the agent to start", func() bool { b, _ := os.ReadFile(pidFile); return len(b) > 0 && clock.waiting() == 1 })
	b, _ := os.ReadFile(pidFile)
	pid := atoi(strings.TrimSpace(string(b)))

	clock.Advance(29 * time.Minute)
	select {
	case <-done:
		t.Fatal("the agent was stopped a minute early")
	case <-time.After(300 * time.Millisecond):
	}
	clock.Advance(time.Minute)
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.note, "1 finding") || !strings.Contains(r.note, "stopped after 30 min") {
			t.Errorf("note %q, err %v", r.note, r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent outlived its time limit")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("agent process %d is still alive", pid)
	}
	if fs, _ := g.st.Findings(context.Background(), 0, 0, "7"); len(fs) != 1 || fs[0].Title != "Found before the limit" {
		t.Errorf("stored: %+v", fs)
	}
}

func TestAgentThatFailsWithoutReportingAnythingFailsTheRunAndSaysWhy(t *testing.T) {
	g := newRig(t)
	_, err := g.explorer(`echo 'Invalid API key · Please run /login'; exit 1`, 5, SystemClock).Run(context.Background(), g.env)
	if err == nil || !strings.Contains(err.Error(), "code 1") || !strings.Contains(err.Error(), "/login") {
		t.Errorf("err = %v", err)
	}
	// An agent that exits badly after reporting still leaves its findings, and the run is not a failure.
	g2 := newRig(t)
	note, err := g2.explorer(`printf '%s\n' '{"severity":"low","title":"x"}' >> "$LOCALCI_FINDINGS"; exit 3`, 5, SystemClock).Run(context.Background(), g2.env)
	if err != nil || !strings.Contains(note, "exited with code 3") {
		t.Errorf("note %q err %v", note, err)
	}
}

// emitted records the events a run emits, keyed by kind.
func (g *rig) emitted() map[string]map[string]any {
	got := map[string]map[string]any{}
	g.env.Emit = func(kind string, data any) {
		if m, ok := data.(map[string]any); ok {
			got[kind] = m
		}
	}
	return got
}

func TestAgentCostAndTurnsAreTheLargestOverEveryResultLine(t *testing.T) {
	g := newRig(t)
	events := g.emitted()
	// The two result lines of a real run: the work (19 turns), then a short closing session that reports a higher
	// running cost but one turn.
	cmd := `echo '{"type":"system"}'
echo '{"type":"result","subtype":"success","total_cost_usd":1.1854,"num_turns":19,"result":"done"}'
echo '{"type":"result","subtype":"success","total_cost_usd":1.2165,"num_turns":1,"result":"ok"}'`
	note, err := g.explorer(cmd, 5, SystemClock).Run(context.Background(), g.env)
	if err != nil || !strings.Contains(note, "$1.22") {
		t.Errorf("note %q err %v", note, err)
	}
	done := events["explore.done"]
	if done["turns"] != 19 || done["cost_usd"] != 1.2165 || done["ended_by"] != nil {
		t.Errorf("explore.done = %v", done)
	}
}

func TestRunThatHitsTheTurnLimitKeepsItsFindingsAndPassesLikeTheTimeLimit(t *testing.T) {
	g := newRig(t)
	events := g.emitted()
	cmd := `printf '%s\n' '{"severity":"high","title":"Found before the limit"}' >> "$LOCALCI_FINDINGS"
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"total_cost_usd":0.9,"num_turns":61}'
exit 1`
	note, err := g.explorer(cmd, 5, SystemClock).Run(context.Background(), g.env)
	if err != nil || !strings.Contains(note, "1 finding") || !strings.Contains(note, "turn limit") {
		t.Errorf("note %q err %v", note, err)
	}
	if done := events["explore.done"]; done["ended_by"] != "turn limit" || done["turns"] != 61 || done["findings"] != 1 {
		t.Errorf("explore.done = %v", done)
	}
	if fs, _ := g.st.Findings(context.Background(), 0, 0, "7"); len(fs) != 1 {
		t.Errorf("stored: %+v", fs)
	}
	// Even a run that found nothing before the limit is a pass, as it is for the time limit.
	g2 := newRig(t)
	if _, err := g2.explorer(`echo '{"type":"result","subtype":"error_max_turns","total_cost_usd":0.5,"num_turns":61}'; exit 1`, 5, SystemClock).Run(context.Background(), g2.env); err != nil {
		t.Errorf("turn limit with no findings failed the run: %v", err)
	}
}

func TestAgentEnvPassesOnlyTheNamedVariablesThatAreSet(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-named")
	g := newRig(t)
	x := g.explorer(`env > "$LOCALCI_OUTPUT_DIR/env"`, 5, SystemClock)
	x.Cfg.Agent.Env = []string{"AWS_REGION", "AWS_NOT_SET_ANYWHERE"}
	if _, err := x.Run(context.Background(), g.env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(g.env.OutDir, "env"))
	got := string(b)
	if !strings.Contains(got, "AWS_REGION=eu-west-1\n") {
		t.Errorf("AWS_REGION was not passed:\n%s", got)
	}
	for _, leak := range []string{"AWS_SECRET_ACCESS_KEY", "AWS_NOT_SET_ANYWHERE"} {
		if strings.Contains(got, leak) {
			t.Errorf("%s reached the agent", leak)
		}
	}
}

func TestAgentEnvRejectsAnythingButAVariableName(t *testing.T) {
	base := map[string]any{"schedule": "0 2 * * *", "suite": "e2e", "base_url": "http://x", "goals": []any{"g"}}
	for _, bad := range []string{"AWS_*", "AWS_REGION=eu", "aws_region", "1AWS", "", "AWS REGION"} {
		sec := map[string]any{}
		for k, v := range base {
			sec[k] = v
		}
		sec["agent"] = map[string]any{"env": []any{bad}}
		if _, err := Parse(sec); err == nil || !strings.Contains(err.Error(), "explore.agent.env[0]") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	sec := map[string]any{"schedule": "0 2 * * *", "suite": "e2e", "base_url": "http://x", "goals": []any{"g"},
		"agent": map[string]any{"env": []any{"AWS_REGION", "CLOUD_ML_REGION", "_X1"}}}
	if c, err := Parse(sec); err != nil || len(c.Agent.Env) != 3 {
		t.Errorf("valid names: %v %v", c, err)
	}
}

func TestAgentGetsTheBriefOnStdinAndTheBaseURLInItsEnvironment(t *testing.T) {
	g := newRig(t)
	cmd := `cat > "$LOCALCI_OUTPUT_DIR/stdin"; echo "$LOCALCI_BASE_URL" > "$LOCALCI_OUTPUT_DIR/base"`
	x := g.explorer(cmd, 5, SystemClock)
	x.Flows = []string{"login (in: user; out: token)"}
	if _, err := x.Run(context.Background(), g.env); err != nil {
		t.Fatal(err)
	}
	in, _ := os.ReadFile(filepath.Join(g.env.OutDir, "stdin"))
	for _, want := range []string{"buy as a new customer", "http://127.0.0.1:4310", "login (in: user; out: token)", "findings.ndjson"} {
		if !strings.Contains(string(in), want) {
			t.Errorf("the brief lacks %q", want)
		}
	}
}

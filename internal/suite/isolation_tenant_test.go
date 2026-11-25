package suite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
)

const tenantKeys = "    isolation: tenant\n    isolation_tenant_flow: create-tenant\n"

// One stack and one database serve every parallel shard, each shard runs the tenant flow, and the run leaves
// no database and no stack process behind.
func TestTenantShardsShareOneStackAndDatabase(t *testing.T) {
	ids := []string{"pass1", "pass2", "pass3", "pass4"}
	g := newRig(t, fakeSuite(tenantKeys), ids, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)

	if ev := isolationEvent(t, g, run.ID); ev == nil || ev["level"] != "tenant" || ev["shared_shards"] != float64(2) {
		t.Fatalf("run.isolation = %v, want tenant on 2 shards", ev)
	}
	dbs, ports := map[string]bool{}, map[string]bool{}
	for _, l := range readLines(filepath.Join(g.shared, "shards.log")) {
		f := strings.Fields(l)
		dbs[f[1]], ports[f[2]] = true, true
	}
	if len(dbs) != 1 || len(ports) != 1 {
		t.Fatalf("shards used databases %v and ports %v, want one of each", dbs, ports)
	}
	stacks := pidsIn(filepath.Join(g.shared, "stack.pids"))
	if len(stacks) != 1 {
		t.Fatalf("%d stacks were started, want one", len(stacks))
	}
	flows := readLines(filepath.Join(g.shared, "tenant.log"))
	if len(flows) != 2 {
		t.Fatalf("tenant flow named to %d shards, want 2: %v", len(flows), flows)
	}
	waitFor(t, "the stack to stop", func() bool { return !alive(stacks[0]) })
	if left := g.dbFiles(); len(left) != 0 {
		t.Fatalf("databases left after the run: %v", left)
	}
}

// A shard whose tenant flow fails never had a tenant: its tests end interrupted with the flow's failure (never
// failed, so never rerun alone and never moved to the serial group), and the run is an infrastructure failure
// that says so in the same words everywhere, however the flow's error looked: one printable line of at most 200
// characters. The other shard finishes its tests, and the interrupted ones are not counted as failed.
func TestInfraTenantFlowFailureIsNotFailedTests(t *testing.T) {
	t.Setenv("FAKE_TENANT_FAIL", "1")
	t.Setenv("FAKE_TENANT_ERROR", "\x1b[31mPOST /api/tenants answered 500: "+strings.Repeat("x", 300)+"\x1b[0m\n    at makeTenant (flows.ts:12)\n")
	ids := []string{"pass1", "pass2", "pass3", "pass4"}
	g := newRig(t, fakeSuite(tenantKeys), ids, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})

	const prefix = "tenant flow create-tenant failed: POST /api/tenants answered 500: x"
	if run.State != "failed" || !strings.HasPrefix(run.Infra, prefix) || len([]rune(run.Infra)) > 200 || strings.ContainsAny(run.Infra, "\x1b\n") {
		t.Fatalf("run %s, infra %q: want failed with the flow's failure as one printable line of at most 200 characters", run.State, run.Infra)
	}
	if run.Reason != "infrastructure: "+run.Infra {
		t.Fatalf("reason %q", run.Reason)
	}
	var ok, broken int
	for _, a := range g.attempts(run.ID) {
		if a.Retry != 0 {
			t.Errorf("%s ran again (attempt %d): a tenant failure must not trigger reruns", a.Test, a.Retry)
		}
		switch {
		case a.Shard == 2 && a.Status == "passed":
			ok++
		case a.Shard == 1 && a.Status == "interrupted":
			broken++
		default:
			t.Errorf("%s on shard %d: %s", a.Test, a.Shard, a.Status)
		}
	}
	if ok != 2 || broken != 2 {
		t.Fatalf("shard 2 passed %d tests and shard 1 was interrupted for %d, want 2 and 2", ok, broken)
	}
	if c, _ := g.st.TestCounts(context.Background(), run.ID); c.Failed != 0 || c.Passed != 2 {
		t.Fatalf("counts %+v: interrupted tests are not failed tests", c)
	}
	fails, _ := g.st.Failures(context.Background(), run.ID)
	evs, _ := g.st.RunEvents(context.Background(), run.ID)
	for _, f := range fails {
		var body struct{ Message string }
		json.Unmarshal(f.Failure, &body)
		if body.Message != run.Infra {
			t.Errorf("%s failure %q, want the run's infrastructure line", f.ID, body.Message)
		}
	}
	var told string
	for _, e := range evs {
		var d struct{ Flow, Error, Reason string }
		json.Unmarshal(e.Data, &d)
		switch e.Kind {
		case "test.serial":
			t.Fatal("a test moved to the serial group for a tenant failure")
		case "tenant.failed":
			told = d.Error
		}
	}
	if !strings.HasPrefix(told, "POST /api/tenants answered 500: x") || len([]rune(told)) > 200 || strings.ContainsAny(told, "\x1b\n") {
		t.Fatalf("tenant.failed carried %q", told)
	}
}

// The shared stack dying under a run (or a shard's own stack) ends the run as an infrastructure failure at once:
// the tests still running end interrupted instead of failing against nothing, no rerun starts on the dead stack,
// and the serial group and the durations stay as they were.
func TestInfraStackKilledMidRunEndsTheRun(t *testing.T) {
	for _, c := range []struct{ name, extra string }{{"shared stack", tenantKeys}, {"a stack per shard", ""}} {
		t.Run(c.name, func(t *testing.T) {
			ids := []string{"slow1", "slow2", "slow3", "slow4", "pass1"}
			g := newRig(t, fakeSuite(c.extra), ids, 4)
			ctx := context.Background()
			g.st.AddSerial(ctx, store.SerialEntry{Suite: "e2e", Test: "pass1"}) // already known: stays
			id, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead()})
			if err != nil {
				t.Fatal(err)
			}
			waitFor(t, "both shards to start their tests", func() bool { return len(readLines(filepath.Join(g.shared, "shards.log"))) >= 2 })
			pids := pidsIn(filepath.Join(g.shared, "stack.pids"))
			if len(pids) == 0 {
				t.Fatal("no stack started")
			}
			killed := time.Now()
			syscall.Kill(pids[0], syscall.SIGKILL)

			run := g.wait(id)
			if took := time.Since(killed); took > 5*time.Second {
				t.Errorf("the run took %s to end after its stack died (the tests alone need 10s)", took)
			}
			if run.State != store.StateFailed || !strings.Contains(run.Infra, "stack app exited before the run ended") {
				t.Fatalf("run %s, infra %q, reason %q", run.State, run.Infra, run.Reason)
			}
			for _, a := range g.attempts(run.ID) {
				if a.Shard == 0 { // the serial group and every rerun run on shard 0
					t.Errorf("%s ran on shard 0 (attempt %d): nothing may start after the death", a.Test, a.Retry)
				}
			}
			if c, _ := g.st.TestCounts(ctx, run.ID); c.Failed != 0 {
				t.Errorf("counts %+v: tests the dead stack never let finish are not failed tests", c)
			}
			if serial, _ := g.st.Serial(ctx, "e2e"); len(serial) != 1 || serial[0].Test != "pass1" {
				t.Errorf("serial group = %+v, want only what was there before", serial)
			}
			if med, _ := g.st.MedianDurations(ctx, ids); len(med) != 0 {
				t.Errorf("durations learned from an infrastructure run: %v", med)
			}
			waitFor(t, "the stacks to stop", func() bool {
				for _, p := range pidsIn(filepath.Join(g.shared, "stack.pids")) {
					if alive(p) {
						return false
					}
				}
				return true
			})
		})
	}
}

// What finalize teaches, it teaches only from a run that learns: with the stack broken the failures it saw are
// not conflicts and the times it measured are not the tests'.
func TestInfraRunTeachesNothing(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	ctx := context.Background()
	at := time.Now()
	done := func(id string, lane int, m mode, status string, from, to time.Duration) attempt {
		return attempt{id: id, lane: lane, mode: m, start: at.Add(from), end: at.Add(to),
			row: store.Test{Run: "5", ID: id, Status: status, MS: int64((to - from) / time.Millisecond)}}
	}
	results := []attempt{
		done("a", 1, modeParallel, "failed", 0, time.Second), // fails beside b, passes alone twice: a conflict
		done("b", 2, modeParallel, "passed", 0, time.Second),
		done("a", 0, modeRerun, "passed", 2*time.Second, 3*time.Second),
		done("a", 0, modeRerun, "passed", 3*time.Second, 4*time.Second),
	}
	learn := func(infra string) ([]store.SerialEntry, map[string]int64) {
		ex := &execution{r: g.r, j: &job{}, suite: g.r.suites["e2e"], id: "5", ctx: ctx, bg: ctx, results: results, infra: infra}
		if err := ex.finalize(); err != nil {
			t.Fatal(err)
		}
		serial, _ := g.st.Serial(ctx, "e2e")
		med, _ := g.st.MedianDurations(ctx, []string{"a", "b"})
		return serial, med
	}
	if serial, med := learn("stack app exited"); len(serial) != 0 || len(med) != 0 {
		t.Fatalf("an infrastructure run taught serial %+v, durations %v", serial, med)
	}
	if serial, med := learn(""); len(serial) != 1 || med["b"] == 0 { // the same results, nothing broken
		t.Fatalf("a healthy run taught serial %+v, durations %v: the test would prove nothing", serial, med)
	}
}

// Once the stack or the tenant flow broke, the hook calls of the run's other shards may not save checkpoints.
func TestHookRunOfAnInfrastructureRunMaySaveNothing(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 1)
	ctx := context.Background()
	ex := &execution{r: g.r, j: &job{}, suite: g.r.suites["e2e"], id: "5", ctx: ctx, bg: ctx}
	if ex.hookRun("a").NoSave {
		t.Fatal("a healthy run may not save")
	}
	ex.infra = "stack app exited"
	if hr := ex.hookRun("a"); !hr.NoSave || hr.Fork {
		t.Fatalf("NoSave=%v Fork=%v after the run turned infrastructure, want only NoSave", hr.NoSave, hr.Fork)
	}
}

// The tenant flow's error reaches the run's reason and the status posted to the forge, so a token in a URL it
// names or a secret it prints must not get there.
func TestInfraTenantFlowFailureHidesURLQueriesAndSecrets(t *testing.T) {
	t.Setenv("FAKE_TENANT_FAIL", "1")
	t.Setenv("FAKE_TENANT_ERROR", "GET https://api.acme.test/tenants?token=abc123 -> 401 (password=hunter2)")
	g := newRig(t, fakeSuite(tenantKeys), []string{"pass1", "pass2"}, 2)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	for _, leaked := range []string{"abc123", "hunter2", "token="} {
		if strings.Contains(run.Infra, leaked) || strings.Contains(run.Reason, leaked) {
			t.Errorf("%q reached the run: infra %q, reason %q", leaked, run.Infra, run.Reason)
		}
	}
	if !strings.Contains(run.Infra, "https://api.acme.test/tenants") {
		t.Errorf("the URL itself should stay readable: %q", run.Infra)
	}
}

// Whatever the reason of an infrastructure failure came from, a stack's last words included, it is scrubbed.
func TestSetInfraScrubsTheReason(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 1)
	ctx := context.Background()
	ex := &execution{r: g.r, j: &job{}, suite: g.r.suites["e2e"], id: "5", ctx: ctx, bg: ctx}
	ex.setInfra("stack api did not start: GET http://localhost:3000/health?token=zzz9 refused")
	if got := ex.infraReason(); strings.Contains(got, "zzz9") || !strings.Contains(got, "http://localhost:3000/health") {
		t.Fatalf("reason %q", got)
	}
}

// The real thing: the sample shop on one stack and one database, a tenant per shard made by its flow.
func TestSampleSuiteRunsOnOneStackWithATenantPerShard(t *testing.T) {
	g := newSampleWith(t, 3, map[string]string{"ACME_STACK": "1"}, `    isolation: tenant
    isolation_tenant_flow: create-tenant
    stack:
      - name: web
        repo: acme/shop
        run: node server.js
        env: {PORT: '{{port "web"}}', DB_PATH: '{{db.url}}', ACME_GLOBAL_SETTINGS: '@SETTINGS@'}
        ready: {http: 'http://127.0.0.1:{{port "web"}}/healthz'}
`)
	var mu sync.Mutex
	dbs, ports, shards := map[string]bool{}, map[int]bool{}, map[int]bool{}
	g.r.OnTest(func(c TestCtx) {
		mu.Lock()
		defer mu.Unlock()
		if c.Shard > 0 {
			dbs[c.DB], ports[c.Ports["web"]], shards[c.Shard] = true, true, true
		}
	}, nil)

	run, _ := g.run()
	if run.State != store.StatePassed {
		t.Fatalf("run %s (%s)", run.State, run.Reason)
	}
	for _, a := range readAttempts(t, g.data, run.ID) {
		if a.Status != "passed" || a.Retry != 0 {
			t.Errorf("%s: %s on attempt %d: sharing the shard's tenant must be enough, no test needs a rerun", a.Test, a.Status, a.Retry)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(shards) < 2 || len(dbs) != 1 || len(ports) != 1 {
		t.Errorf("%d shards ran on %d databases and %d ports, want one of each", len(shards), len(dbs), len(ports))
	}
	// One tenant per shard: the flow ran once for each shard, however many tests the shard had.
	evs, err := g.st.RunEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	made := 0
	for _, e := range evs {
		if e.Kind == "flow.begin" && e.Shard > 0 && strings.Contains(string(e.Data), `"create-tenant"`) {
			made++
		}
	}
	if made != len(shards) {
		t.Errorf("the tenant flow ran %d times for %d shards", made, len(shards))
	}
}

// Shard 0 (the serial group and the reruns) runs several test commands against one stack and database of its own:
// its tenant is made once, by the first of them. Making it again would collide with the tenant already there.
func TestShardZeroMakesItsTenantOnceAcrossItsTestCommands(t *testing.T) {
	g := newRig(t, fakeSuite(tenantKeys), []string{"pass1", "pass2", "setter1", "reader1"}, 4)
	if err := g.st.AddSerial(context.Background(), store.SerialEntry{Suite: "e2e", Test: "pass2"}); err != nil {
		t.Fatal(err)
	}
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()})) // pass2 in the serial group, reader1 rerun alone
	var zero int
	for _, l := range readLines(filepath.Join(g.shared, "tenant.log")) {
		if strings.HasPrefix(l, "0 ") {
			zero++
		}
	}
	if zero != 1 {
		t.Fatalf("shard 0 ran the tenant flow %d times, want once", zero)
	}
}

// When the one stack of a tenant run does not start, the run fails and the shared database made for it is dropped.
func TestTenantStackThatDoesNotStartLeavesNoDatabase(t *testing.T) {
	yaml := strings.Replace(fakeSuite(tenantKeys), `echo up {{port "app"}}; exec sleep 300`, `exit 3`, 1)
	g := newRig(t, yaml, []string{"pass1", "pass2"}, 4)
	if run := g.run(Request{Suite: "e2e", Heads: mainHead()}); run.State == store.StatePassed {
		t.Fatalf("a run whose stack never started passed: %+v", run)
	}
	if left := g.dbFiles(); len(left) != 0 {
		t.Fatalf("databases left after the run: %v", left)
	}
}

// With `shards: auto` a tenant run counts its shards with the stack paid once, which fits more of them than an
// isolated count. All of them run here against the one stack, even with workers to hand shards to.
func TestAutoTenantShardsAllRunHere(t *testing.T) {
	yaml := strings.Replace(fakeSuite(tenantKeys), "    shards: 2\n", "    shards: auto\n", 1)
	g := newRig(t, yaml, []string{"pass1", "pass2", "pass3", "pass4"}, 4)
	g.r.d.Sched = sched.New(4, 3<<30) // two isolated shards of 1.5 GiB fit, three tenant ones do
	fr := &fakeRemote{t: t, slots: 1}
	g.r.d.Remote = fr

	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if jobs := fr.jobsSnapshot(); len(jobs) != 0 {
		t.Fatalf("%d tenant shard(s) went to a worker, want every shard here", len(jobs))
	}
	g.mustPass(run)
	if ev := isolationEvent(t, g, run.ID); ev == nil || ev["level"] != "tenant" || ev["shared_shards"] != float64(3) {
		t.Fatalf("run.isolation = %v, want tenant on 3 shards", ev)
	}
}

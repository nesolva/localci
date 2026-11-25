package suite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// twoSuites is the fake suite twice: "e2e" with extra lines, and "next" behind it in the queue.
func twoSuites(first string) string {
	body := func(name, extra string) string {
		return strings.Replace(strings.TrimPrefix(fakeSuite(extra), "suites:\n"), "  e2e:", "  "+name+":", 1)
	}
	return "suites:\n" + body("e2e", first) + body("next", "")
}

// lostWorker gives a worker's shard of suite "e2e" back as lost, so it falls back to this machine; other suites'
// shards run on the fake worker.
func lostWorker(t *testing.T) *fakeRemote {
	fr := &fakeRemote{t: t, slots: 1}
	fr.script = func(_ int, job RemoteJob, sink RemoteSink) (RemoteResult, error) {
		if job.Suite == "e2e" {
			return RemoteResult{}, ErrNoWorker
		}
		return fr.runFake(job, sink)
	}
	return fr
}

// waitWithin waits for a run, failing fast instead of at the rig's 90 s: a deadlocked requeue must read as one.
func (g *rig) waitWithin(id string, d time.Duration) store.Run {
	g.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	run, err := g.r.Wait(ctx, id)
	if err != nil {
		g.t.Fatalf("run %s did not finish in %s (a requeued shard waiting on its own run's grant?): %v", id, d, err)
	}
	return run
}

func runBehind(t *testing.T, g *rig) {
	t.Helper()
	first, err := g.r.Submit(Request{Suite: "e2e", Heads: mainHead()})
	if err != nil {
		t.Fatal(err)
	}
	second, err := g.r.Submit(Request{Suite: "next", Heads: mainHead()})
	if err != nil {
		t.Fatal(err)
	}
	g.mustPass(g.waitWithin(first, 40*time.Second))
	g.mustPass(g.waitWithin(second, 40*time.Second))
}

// With `shards: auto` the run's grant fills the machine. A shard that falls back from a lost worker used to ask the
// scheduler for one more shard and wait for its own run to end, holding up the queue behind it.
func TestRequeuedShardRunsInsideItsGrant(t *testing.T) {
	yaml := strings.Replace(twoSuites(""), "shards: 2", "shards: auto", 1)
	g := newRig(t, yaml, manyTests(), 4)
	g.r.d.Remote = lostWorker(t)
	runBehind(t, g)
	if peak := g.sc.Stats().PeakCPUs; peak != 4 {
		t.Fatalf("the run was meant to fill the 4 CPUs; peak %d", peak)
	}
}

// An exclusive suite holds the whole machine for its run; its requeued shard runs inside that.
func TestRequeuedShardExclusive(t *testing.T) {
	g := newRig(t, twoSuites("    exclusive: true\n"), manyTests(), 4)
	g.r.d.Remote = lostWorker(t)
	runBehind(t, g)
}

// With every shard remote there is still one slot, so a requeue never waits on an empty pool. No configuration lays
// a run out with no local shard, so this drives the pool a run opens for that layout.
func TestRequeuedShardAllRemote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex := &execution{ctx: ctx, localShards: 0}
	ex.openSlots()
	free, err := ex.takeSlot()
	if err != nil {
		t.Fatalf("a requeue found no slot with every shard remote: %v", err)
	}
	waiting := make(chan error, 1)
	go func() { _, err := ex.takeSlot(); waiting <- err }()
	select {
	case err := <-waiting:
		t.Fatalf("a second requeue got a slot while the only one was taken (%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	free()
	free() // giving a slot back twice must not open a second one
	if err := <-waiting; err != nil {
		t.Fatalf("the waiting requeue got no slot once it was free: %v", err)
	}
	if len(ex.slots) != 1 {
		t.Fatalf("pool holds %d slots in use, want the one the waiter took", len(ex.slots))
	}
	cancel()
	if _, err := ex.takeSlot(); err == nil {
		t.Fatal("a cancelled run took a slot")
	}
}

func mustSubmit(t *testing.T, g *rig, req Request) string {
	t.Helper()
	id, err := g.r.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (g *rig) capacityIsFree() bool {
	st := g.sc.Stats()
	return st.UsedCPUs == 0 && st.UsedMemory == 0 && st.Running == 0
}

// Whatever happens to the run, its grant goes back exactly once: nothing stays admitted, and nothing is released
// twice (the next full-machine admission would then see a negative balance and run beside another run).
func TestRequeuedShardReleasesOnce(t *testing.T) {
	t.Run("both finish", func(t *testing.T) {
		yaml := strings.Replace(twoSuites(""), "shards: 2", "shards: auto", 1)
		g := newRig(t, yaml, manyTests(), 4)
		g.r.d.Remote = lostWorker(t)
		runBehind(t, g)
		waitFor(t, "the grants to be given back", g.capacityIsFree)
	})
	t.Run("exclusive", func(t *testing.T) {
		g := newRig(t, twoSuites("    exclusive: true\n"), manyTests(), 4)
		g.r.d.Remote = lostWorker(t)
		runBehind(t, g)
		waitFor(t, "the grants to be given back", g.capacityIsFree)
	})
	t.Run("cancelled while the requeue waits for a slot", func(t *testing.T) {
		// One local shard that hangs holds the run's only slot; the shard that fell back from the worker waits for it.
		g := newRig(t, fakeSuite(""), []string{"hang1", "hang2"}, 4)
		fr := lostWorker(t)
		g.r.d.Remote = fr
		id := mustSubmit(t, g, Request{Suite: "e2e", Heads: mainHead()})
		waitFor(t, "the local shard to hang and the worker to hand its shard back", func() bool {
			return len(pidsIn(filepath.Join(g.shared, "pids"))) == 1 && len(fr.jobsSnapshot()) == 1
		})
		time.Sleep(300 * time.Millisecond)
		if n := len(pidsIn(filepath.Join(g.shared, "pids"))); n != 1 {
			t.Fatalf("the requeued shard ran beside the local one (%d hanging tests): it took more than the grant", n)
		}
		if !g.r.Cancel(id) {
			t.Fatal("Cancel said the run was not active")
		}
		if run := g.waitWithin(id, 40*time.Second); run.State != store.StateCancelled {
			t.Fatalf("run is %s (%s), want cancelled", run.State, run.Reason)
		}
		waitFor(t, "the grant to be given back", g.capacityIsFree)
	})
}

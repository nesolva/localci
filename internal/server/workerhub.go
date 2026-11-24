package server

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// DefaultClaimWait is how long a shard waits for a worker to take it before it runs on this machine.
const DefaultClaimWait = 20 * time.Second

// liveWorker is what the primary knows about a worker that is connected now.
type liveWorker struct {
	capacity int
	seen     time.Time
	shards   []string // keys of the shards it holds
}

// assignment is one shard handed (or about to be handed) to a worker.
type assignment struct {
	key     string
	job     suite.RemoteJob
	sink    suite.RemoteSink
	worker  string    // "" until a worker claims it
	since   time.Time // when the worker claimed it
	claimed chan struct{}
	done    chan outcome
}

type outcome struct {
	res suite.RemoteResult
	err error
}

func keyOf(run string, shard, gen int) string { return fmt.Sprintf("%s/%d/%d", run, shard, gen) }

// WorkerHub tracks connected workers and gives them shards by capacity. It is the suite runner's Remote.
type WorkerHub struct {
	LostAfter time.Duration // a worker silent this long loses its shards
	ClaimWait time.Duration // how long a shard waits for a worker

	mu      sync.Mutex
	live    map[string]*liveWorker
	revoked map[string]bool // names dropped by a revocation: a late call of theirs creates nothing
	queue   []*assignment
	active  map[string]*assignment
	changed chan struct{}
	now     func() time.Time
}

// NewWorkerHub makes a hub; lostAfter zero means DefaultLostAfter.
func NewWorkerHub(lostAfter time.Duration) *WorkerHub {
	if lostAfter <= 0 {
		lostAfter = DefaultLostAfter
	}
	return &WorkerHub{LostAfter: lostAfter, ClaimWait: DefaultClaimWait, live: map[string]*liveWorker{}, revoked: map[string]bool{},
		active: map[string]*assignment{}, changed: make(chan struct{}), now: time.Now}
}

func (h *WorkerHub) signalLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

func (h *WorkerHub) aliveLocked(name string) bool {
	lw := h.live[name]
	return lw != nil && h.now().Sub(lw.seen) < h.LostAfter
}

func (h *WorkerHub) heldLocked(name string) int {
	n := 0
	for _, a := range h.active {
		if a.worker == name {
			n++
		}
	}
	return n
}

// beat records that a worker was heard from, with the capacity it advertises. It reports false, and records
// nothing, for a worker that was revoked.
func (h *WorkerHub) beat(name string, capacity int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.revoked[name] {
		return false
	}
	lw := h.live[name]
	if lw == nil {
		lw = &liveWorker{}
		h.live[name] = lw
	}
	lw.capacity, lw.seen = min(max(capacity, 1), store.MaxWorkerCapacity), h.now()
	h.signalLocked()
	return true
}

// heartbeat is what the primary does with a worker's heartbeat: it records the worker as alive, requeues the shards
// it holds that the worker does not list as running (it restarted, lost the claim answer, or gave up shipping the
// output: it would heartbeat forever and the run would hang), and returns the listed keys it must stop. ok is false
// for a revoked worker, which gets nothing.
// A shard claimed a moment ago gets a grace: the worker may not have listed it yet.
func (h *WorkerHub) heartbeat(name string, capacity int, running []string) (drop []string, ok bool) {
	if !h.beat(name, capacity) {
		return nil, false
	}
	h.mu.Lock()
	var lost []*assignment
	for _, a := range h.active {
		if a.worker == name && !slices.Contains(running, a.key) && h.now().Sub(a.since) > h.LostAfter/2 {
			lost = append(lost, a)
		}
	}
	h.mu.Unlock()
	for _, a := range lost {
		h.finish(a, suite.RemoteResult{}, suite.ErrWorkerLost)
	}
	return h.unknownTo(name, running), true
}

func (h *WorkerHub) touch(name string) {
	h.mu.Lock()
	if lw := h.live[name]; lw != nil {
		lw.seen = h.now()
	}
	h.mu.Unlock()
}

// snapshot returns a worker's live state and whether it is connected.
func (h *WorkerHub) snapshot(name string) (liveWorker, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	lw := h.live[name]
	if lw == nil {
		return liveWorker{}, false
	}
	c := *lw
	c.shards = nil
	for k, a := range h.active {
		if a.worker == name {
			c.shards = append(c.shards, k)
		}
	}
	slices.Sort(c.shards)
	return c, h.aliveLocked(name)
}

// unknownTo returns the keys among running that this worker no longer holds: finished, cancelled or requeued.
func (h *WorkerHub) unknownTo(name string, running []string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	drop := []string{}
	for _, k := range running {
		if a := h.active[k]; a == nil || a.worker != name {
			drop = append(drop, k)
		}
	}
	return drop
}

// dropWorker forgets a worker (revoked): its shards are lost at once.
func (h *WorkerHub) dropWorker(name string) {
	h.mu.Lock()
	delete(h.live, name)
	h.revoked[name] = true
	var lost []*assignment
	for _, a := range h.active {
		if a.worker == name {
			lost = append(lost, a)
		}
	}
	h.mu.Unlock()
	for _, a := range lost {
		h.finish(a, suite.RemoteResult{}, suite.ErrWorkerLost)
	}
}

// Slots is how many more shards connected workers can take right now.
func (h *WorkerHub) Slots() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	free := 0
	for name, lw := range h.live {
		if h.aliveLocked(name) {
			free += max(lw.capacity-h.heldLocked(name), 0)
		}
	}
	return max(free-len(h.queue), 0)
}

// RunShard queues the shard for a worker and waits for it to finish. It returns ErrNoWorker when none takes it in
// ClaimWait, ErrWorkerLost when the worker that took it goes silent or is revoked.
func (h *WorkerHub) RunShard(ctx context.Context, job suite.RemoteJob, sink suite.RemoteSink) (suite.RemoteResult, error) {
	a := &assignment{key: keyOf(job.Run, job.Shard, job.Gen), job: job, sink: sink, claimed: make(chan struct{}), done: make(chan outcome, 1)}
	h.mu.Lock()
	h.queue = append(h.queue, a)
	h.signalLocked()
	h.mu.Unlock()
	defer h.remove(a)

	timer := time.NewTimer(h.ClaimWait)
	defer timer.Stop()
	select {
	case <-a.claimed:
	case <-timer.C:
		select {
		case <-a.claimed: // claimed at the last moment
		default:
			return suite.RemoteResult{}, suite.ErrNoWorker
		}
	case <-ctx.Done():
		return suite.RemoteResult{}, ctx.Err()
	}
	tick := time.NewTicker(max(h.LostAfter/10, 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case o := <-a.done:
			return o.res, o.err
		case <-tick.C:
			h.mu.Lock()
			gone := !h.aliveLocked(a.worker)
			h.mu.Unlock()
			if gone {
				return suite.RemoteResult{}, suite.ErrWorkerLost
			}
		case <-ctx.Done():
			return suite.RemoteResult{}, ctx.Err()
		}
	}
}

// remove takes the assignment out of the queue and the active set, so a late message of the worker is refused.
func (h *WorkerHub) remove(a *assignment) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := slices.Index(h.queue, a); i >= 0 {
		h.queue = slices.Delete(h.queue, i, i+1)
	}
	delete(h.active, a.key)
	h.signalLocked()
}

func (h *WorkerHub) finish(a *assignment, res suite.RemoteResult, err error) {
	select {
	case a.done <- outcome{res, err}:
	default:
	}
}

// claim waits up to wait for a shard the worker has room for.
func (h *WorkerHub) claim(ctx context.Context, name string, wait time.Duration) *suite.RemoteJob {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		if lw := h.live[name]; lw != nil {
			lw.seen = h.now()
			if len(h.queue) > 0 && h.heldLocked(name) < lw.capacity {
				a := h.queue[0]
				h.queue = h.queue[1:]
				a.worker, a.since = name, h.now()
				h.active[a.key] = a
				close(a.claimed)
				h.mu.Unlock()
				return &a.job
			}
		}
		ch := h.changed
		h.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		}
	}
}

// assignmentOf finds the shard a worker says it is working on, if the primary still has it assigned to that worker.
func (h *WorkerHub) assignmentOf(name, run string, shard, gen int) *assignment {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a := h.active[keyOf(run, shard, gen)]; a != nil && a.worker == name {
		return a
	}
	return nil
}

// Package sched admits work against the machine's capacity. Suites and PR checks ask for what they need
// (CPUs and memory) and wait in one first-in-first-out line, so total admitted work never exceeds capacity and a
// large request is never starved by a stream of small ones. An exclusive request waits for everything running to
// finish and holds back all later work while it runs.
package sched

import (
	"context"
	"sync"
)

// Cost is what a piece of work needs while it runs.
type Cost struct {
	CPUs   int
	Memory uint64 // bytes
}

// Admitter is what runners code against; *Scheduler implements it.
type Admitter interface {
	// Admit blocks until cost fits free capacity, then returns the function that gives it back.
	Admit(ctx context.Context, c Cost) (release func(), err error)
}

// Scheduler is safe for concurrent use.
type Scheduler struct {
	mu     sync.Mutex
	cpus   int    // capacity; always at least 1
	memory uint64 // capacity in bytes; 0 = not limited
	docker uint64 // memory of the Docker VM in bytes; 0 = unknown or no Docker. Not admitted against: it only informs shard counts

	usedCPUs   int
	usedMemory uint64
	running    int
	exclusive  bool
	queue      []*waiter
	peakCPUs   int
}

type waiter struct {
	cost      Cost
	exclusive bool
	granted   bool
	ready     chan struct{}
}

// New returns a scheduler for a machine with cpus logical CPUs and memory bytes available (0 = unknown, not
// limited).
func New(cpus int, memory uint64) *Scheduler {
	return &Scheduler{cpus: max(cpus, 1), memory: memory}
}

// WithDockerMemory records the Docker VM's memory, which bounds how many stacks fit although the host has more
// free; it returns the scheduler.
func (s *Scheduler) WithDockerMemory(bytes uint64) *Scheduler {
	s.docker = bytes
	return s
}

// DockerMemory is the Docker VM's memory in bytes, 0 when unknown.
func (s *Scheduler) DockerMemory() uint64 { return s.docker }

// Capacity is the total the scheduler hands out.
func (s *Scheduler) Capacity() Cost { return Cost{CPUs: s.cpus, Memory: s.memory} }

// Stats is a snapshot for the queue view and for tests.
type Stats struct {
	Capacity   Cost
	UsedCPUs   int
	UsedMemory uint64
	Running    int
	Waiting    int
	PeakCPUs   int // highest UsedCPUs seen
}

// Stats returns the current usage.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Capacity: s.Capacity(), UsedCPUs: s.usedCPUs, UsedMemory: s.usedMemory, Running: s.running,
		Waiting: len(s.queue), PeakCPUs: s.peakCPUs}
}

// Admit waits in line until c fits. A cost larger than the machine is cut to the machine, so it runs alone
// instead of waiting for ever. Cancelling ctx leaves the line.
func (s *Scheduler) Admit(ctx context.Context, c Cost) (func(), error) {
	return s.admit(ctx, &waiter{cost: c})
}

// AdmitExclusive waits until nothing else is running, then holds the whole machine: later requests wait until
// the returned release is called.
func (s *Scheduler) AdmitExclusive(ctx context.Context) (func(), error) {
	return s.admit(ctx, &waiter{cost: s.Capacity(), exclusive: true})
}

func (s *Scheduler) admit(ctx context.Context, w *waiter) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	w.cost.CPUs = min(max(w.cost.CPUs, 0), s.cpus)
	if s.memory > 0 {
		w.cost.Memory = min(w.cost.Memory, s.memory)
	}
	w.ready = make(chan struct{})

	s.mu.Lock()
	s.queue = append(s.queue, w)
	s.dispatch()
	s.mu.Unlock()

	select {
	case <-w.ready:
		return s.releaser(w), nil
	case <-ctx.Done():
		s.mu.Lock()
		granted := w.granted
		if !granted {
			s.remove(w)
			s.dispatch() // the line may have been blocked behind this one
		}
		s.mu.Unlock()
		if granted {
			s.releaser(w)()
		}
		return nil, ctx.Err()
	}
}

func (s *Scheduler) releaser(w *waiter) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.usedCPUs -= w.cost.CPUs
			s.usedMemory -= w.cost.Memory
			s.running--
			if w.exclusive {
				s.exclusive = false
			}
			s.dispatch()
		})
	}
}

func (s *Scheduler) remove(w *waiter) {
	for i, q := range s.queue {
		if q == w {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

// dispatch grants the head of the line while it fits. Strict order is what keeps a large request from starving.
// The caller holds mu.
func (s *Scheduler) dispatch() {
	for len(s.queue) > 0 && !s.exclusive {
		w := s.queue[0]
		if w.exclusive {
			if s.running > 0 {
				return
			}
		} else if s.usedCPUs+w.cost.CPUs > s.cpus || (s.memory > 0 && s.usedMemory+w.cost.Memory > s.memory) {
			return
		}
		s.queue = s.queue[1:]
		s.usedCPUs += w.cost.CPUs
		s.usedMemory += w.cost.Memory
		s.running++
		s.exclusive = w.exclusive
		s.peakCPUs = max(s.peakCPUs, s.usedCPUs)
		w.granted = true
		close(w.ready)
	}
}

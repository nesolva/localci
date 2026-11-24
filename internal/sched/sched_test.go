package sched

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out: %s", what)
	}
}

// Random requests of every size: admitted work never exceeds the machine and every request is served.
func TestAdmitNeverExceedsCapacityAndNothingStarves(t *testing.T) {
	const cpus, mem = 6, 6 << 30
	s := New(cpus, mem)
	rng := rand.New(rand.NewSource(7))
	var inCPU, inMem atomic.Int64
	var wg sync.WaitGroup
	var served atomic.Int64
	costs := make([]Cost, 300)
	for i := range costs {
		costs[i] = Cost{CPUs: 1 + rng.Intn(8), Memory: uint64(rng.Intn(4)+1) << 30} // up to more than the machine
	}
	within(t, 20*time.Second, "all requests served", func() {
		for _, c := range costs {
			wg.Add(1)
			go func(c Cost) {
				defer wg.Done()
				release, err := s.Admit(context.Background(), c)
				if err != nil {
					t.Error(err)
					return
				}
				c.CPUs = min(c.CPUs, cpus)
				cpu, m := inCPU.Add(int64(c.CPUs)), inMem.Add(int64(c.Memory))
				if cpu > cpus || m > mem {
					t.Errorf("over capacity: %d cpus, %d bytes", cpu, m)
				}
				time.Sleep(time.Duration(c.CPUs%3) * time.Millisecond)
				inCPU.Add(-int64(c.CPUs))
				inMem.Add(-int64(c.Memory))
				served.Add(1)
				release()
			}(c)
		}
		wg.Wait()
	})
	if served.Load() != int64(len(costs)) {
		t.Fatalf("served %d of %d", served.Load(), len(costs))
	}
	if st := s.Stats(); st.UsedCPUs != 0 || st.Running != 0 || st.Waiting != 0 || st.PeakCPUs > cpus {
		t.Fatalf("stats after drain = %+v", st)
	}
}

// A large request is not overtaken by a stream of small ones that would keep the machine busy for ever.
func TestLargeRequestIsNotStarved(t *testing.T) {
	s := New(4, 0)
	releaseSmall, _ := s.Admit(context.Background(), Cost{CPUs: 1})
	bigDone := make(chan struct{})
	go func() {
		r, _ := s.Admit(context.Background(), Cost{CPUs: 4})
		close(bigDone)
		r()
	}()
	waitFor(t, func() bool { return s.Stats().Waiting == 1 })
	// A small request arriving after the big one must queue behind it although it would fit now.
	late := make(chan struct{})
	go func() {
		r, _ := s.Admit(context.Background(), Cost{CPUs: 1})
		close(late)
		r()
	}()
	waitFor(t, func() bool { return s.Stats().Waiting == 2 })
	select {
	case <-late:
		t.Fatal("a later small request overtook the large one")
	case <-time.After(50 * time.Millisecond):
	}
	releaseSmall()
	within(t, 2*time.Second, "big then late served", func() { <-bigDone; <-late })
}

func TestExclusiveWaitsForRunningAndHoldsNewWork(t *testing.T) {
	s := New(4, 0)
	r1, _ := s.Admit(context.Background(), Cost{CPUs: 1})
	excl := make(chan func(), 1)
	go func() {
		r, _ := s.AdmitExclusive(context.Background())
		excl <- r
	}()
	waitFor(t, func() bool { return s.Stats().Waiting == 1 })
	select {
	case <-excl:
		t.Fatal("exclusive started while work was running")
	case <-time.After(50 * time.Millisecond):
	}
	r1()
	var rx func()
	within(t, 2*time.Second, "exclusive admitted", func() { rx = <-excl })

	other := make(chan struct{})
	go func() {
		r, _ := s.Admit(context.Background(), Cost{CPUs: 1})
		close(other)
		r()
	}()
	select {
	case <-other:
		t.Fatal("new work ran next to an exclusive suite")
	case <-time.After(50 * time.Millisecond):
	}
	rx()
	within(t, 2*time.Second, "work admitted after exclusive", func() { <-other })
}

// A request that gives up while waiting leaves the line and does not block the ones behind it.
func TestCancelledWaiterLeavesTheLine(t *testing.T) {
	s := New(2, 0)
	hold, _ := s.Admit(context.Background(), Cost{CPUs: 2})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := s.Admit(ctx, Cost{CPUs: 2}); errc <- err }()
	waitFor(t, func() bool { return s.Stats().Waiting == 1 })
	behind := make(chan struct{})
	go func() { r, _ := s.Admit(context.Background(), Cost{CPUs: 1}); close(behind); r() }()
	waitFor(t, func() bool { return s.Stats().Waiting == 2 })
	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	hold()
	within(t, 2*time.Second, "waiter behind served", func() { <-behind })
	waitFor(t, func() bool { return s.Stats().UsedCPUs == 0 })
}

func TestReleaseTwiceGivesBackOnce(t *testing.T) {
	s := New(2, 0)
	r, _ := s.Admit(context.Background(), Cost{CPUs: 1})
	other, _ := s.Admit(context.Background(), Cost{CPUs: 1})
	r()
	r()
	if st := s.Stats(); st.UsedCPUs != 1 {
		t.Fatalf("used = %d, want 1 (the other holder)", st.UsedCPUs)
	}
	other()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

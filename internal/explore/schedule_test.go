package explore

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// fakeRunner records submissions; a run stays "running" until release is closed.
type fakeRunner struct {
	mu      sync.Mutex
	started []suite.Request
	release chan struct{}
}

func (f *fakeRunner) Submit(r suite.Request) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, r)
	return "r" + string(rune('0'+len(f.started))), nil
}
func (f *fakeRunner) Wait(ctx context.Context, id string) (store.Run, error) {
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	return store.Run{}, nil
}
func (f *fakeRunner) Suite(string) (*suite.Suite, bool) { return &suite.Suite{}, true }
func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func TestScheduleFiresAtTheConfiguredTimeAndSkipsANightThatOverruns(t *testing.T) {
	g := newRig(t)
	clock := newClock(time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC))
	r := &fakeRunner{release: make(chan struct{})}
	s, err := New(Config{Suite: "e2e", Schedule: "0 2 * * *", BaseURL: "http://127.0.0.1:1"}, r, g.st)
	if err != nil {
		t.Fatal(err)
	}
	s.Clock = clock
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Schedule(ctx)

	eventually(t, "the schedule to wait for 02:00", func() bool { return clock.waiting() == 1 })
	clock.Advance(59 * time.Minute) // 01:59
	time.Sleep(50 * time.Millisecond)
	if n := r.count(); n != 0 {
		t.Fatalf("started %d runs before 02:00", n)
	}
	clock.Advance(time.Minute) // 02:00
	eventually(t, "the 02:00 run", func() bool { return r.count() == 1 })
	if got := r.started[0]; got.Trigger != suite.ExploreTrigger || got.RequestedBy != "schedule" || got.Explore == nil {
		t.Fatalf("scheduled request = %+v", got)
	}

	// The run is still going when the next night comes: that night is skipped, not queued.
	eventually(t, "the schedule to wait for the next night", func() bool { return clock.waiting() == 1 })
	clock.Advance(24 * time.Hour)
	time.Sleep(50 * time.Millisecond)
	if n := r.count(); n != 1 {
		t.Fatalf("a second run started while the first was active: %d", n)
	}

	close(r.release) // the first run ends
	eventually(t, "the service to be idle", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active == "" })
	eventually(t, "the schedule to wait again", func() bool { return clock.waiting() == 1 })
	clock.Advance(24 * time.Hour)
	eventually(t, "the next night's run", func() bool { return r.count() == 2 })
}

func TestCronNext(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct{ expr, from, want string }{
		{"0 2 * * *", "2026-03-01 02:00", "2026-03-02 02:00"}, // strictly after
		{"0 2 * * *", "2026-03-01 01:59", "2026-03-01 02:00"},
		{"*/15 * * * *", "2026-03-01 10:07", "2026-03-01 10:15"},
		{"30 9 * * 1-5", "2026-03-06 10:00", "2026-03-09 09:30"}, // Friday -> Monday
		{"0 0 1,15 * *", "2026-03-02 00:00", "2026-03-15 00:00"},
		{"0 0 * * 7", "2026-03-02 00:00", "2026-03-08 00:00"},  // 7 is Sunday
		{"0 0 13 * 5", "2026-03-01 00:00", "2026-03-06 00:00"}, // day-of-month or weekday
		{"0 8 29 2 *", "2026-03-01 00:00", "2028-02-29 08:00"}, // leap day
	} {
		cr, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%q: %v", c.expr, err)
		}
		if got := cr.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%q after %s = %s, want %s", c.expr, c.from, got.Format("2006-01-02 15:04"), c.want)
		}
	}
	never, _ := ParseCron("0 0 31 2 *")
	if got := never.Next(at("2026-03-01 00:00")); !got.IsZero() {
		t.Errorf("31 February fired at %s", got)
	}
}

func TestParseCronRejectsWhatCouldNeverRun(t *testing.T) {
	for _, expr := range []string{"", "0 2 * *", "60 2 * * *", "0 24 * * *", "0 2 0 * *", "0 2 * 13 *", "0 2 * * 8", "*/0 * * * *", "5-1 * * * *", "a * * * *"} {
		if _, err := ParseCron(expr); err == nil {
			t.Errorf("%q was accepted", expr)
		}
	}
}

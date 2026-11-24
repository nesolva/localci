package explore

import (
	"testing"
	"time"
)

// nextWithin runs Next and fails the test when it does not come back: a schedule that hangs stalls the daemon.
func nextWithin(t *testing.T, c *Cron, from time.Time) time.Time {
	t.Helper()
	done := make(chan time.Time, 1)
	go func() { done <- c.Next(from) }()
	select {
	case n := <-done:
		return n
	case <-time.After(3 * time.Second):
		t.Fatalf("Next(%v) did not return", from)
		return time.Time{}
	}
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	return loc
}

func TestCronNextDoesNotHangWhenTheHourDoesNotExistOnASpringForwardDay(t *testing.T) {
	loc := zone(t, "America/New_York")
	c, _ := ParseCron("0 2 * * *")
	from := time.Date(2026, 3, 8, 0, 30, 0, 0, loc)
	n := nextWithin(t, c, from)
	if !n.After(from) {
		t.Fatalf("Next went backwards: %v", n)
	}
	if n.Day() != 9 || n.Hour() != 2 {
		t.Fatalf("Next = %v, want 2:00 the day after the skipped one", n)
	}
}

func TestCronNextStaysMonotonicThroughAFallBackRepeatedHour(t *testing.T) {
	loc := zone(t, "America/New_York")
	c, _ := ParseCron("*/30 * * * *")
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	prev := t0
	for i := 0; i < 8; i++ { // 00:30 ... through 03:30, crossing 01:00-02:00 twice
		n := nextWithin(t, c, prev)
		if n.Sub(prev) != 30*time.Minute {
			t.Fatalf("step %d: %v -> %v is not 30 minutes", i, prev, n)
		}
		prev = n
	}
}

func TestCronNextStepsToTheNextDayWhenMidnightIsInADstGap(t *testing.T) {
	loc := zone(t, "America/Havana") // clocks jump 00:00 to 01:00 on the second Sunday of March
	c, _ := ParseCron("0 0 * * *")
	from := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	n := nextWithin(t, c, from)
	if !n.After(from) || n.Day() != 9 || n.Hour() != 0 {
		t.Fatalf("Next = %v, want 00:00 on the 9th", n)
	}
}

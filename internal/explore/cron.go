package explore

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field schedule (minute hour day-of-month month day-of-week) in the time zone of the time it
// is asked about. Fields take `*`, numbers, ranges `a-b`, steps `*/n` or `a-b/n` and comma lists; day-of-week is
// 0 to 7 (0 and 7 are Sunday). As in cron, when both day fields are restricted a day matches if either does.
type Cron struct {
	min, hour, dom, month, dow uint64 // bit sets
	domAny, dowAny             bool
}

// ParseCron reads a schedule.
func ParseCron(expr string) (*Cron, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("%q is not a cron expression of five fields (minute hour day month weekday), e.g. 0 2 * * *", expr)
	}
	c := &Cron{}
	var err error
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	names := [5]string{"minute", "hour", "day of month", "month", "day of week"}
	sets := [5]*uint64{&c.min, &c.hour, &c.dom, &c.month, &c.dow}
	for i, field := range f {
		if *sets[i], err = parseField(field, bounds[i][0], bounds[i][1]); err != nil {
			return nil, fmt.Errorf("%s %q: %w", names[i], field, err)
		}
	}
	if c.dow&(1<<7) != 0 { // 7 is Sunday too
		c.dow |= 1
	}
	c.domAny, c.dowAny = f[2] == "*", f[4] == "*"
	return c, nil
}

func parseField(s string, lo, hi int) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(s, ",") {
		rng, step := part, 1
		if r, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n < 1 {
				return 0, fmt.Errorf("step %q is not a positive number", st)
			}
			rng, step = r, n
		}
		a, b := lo, hi
		if rng != "*" {
			from, to, isRange := strings.Cut(rng, "-")
			var err error
			if a, err = strconv.Atoi(from); err != nil {
				return 0, fmt.Errorf("%q is not a number", from)
			}
			b = a
			if isRange {
				if b, err = strconv.Atoi(to); err != nil {
					return 0, fmt.Errorf("%q is not a number", to)
				}
			} else if step > 1 {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return 0, fmt.Errorf("%d-%d is outside %d-%d", a, b, lo, hi)
		}
		for v := a; v <= b; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dow
	case c.dowAny:
		return dom
	}
	return dom || dow
}

// Next is the first matching minute strictly after t, in t's time zone, or the zero time when nothing matches in
// the next five years (a day that never exists, such as the 31st of February).
func (c *Cron) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		switch {
		case c.month&(1<<int(t.Month())) == 0:
			t = forward(t, time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location()))
		case !c.dayMatches(t):
			t = forward(t, time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location()))
		case c.hour&(1<<t.Hour()) == 0:
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute) // to the next full hour on the clock, in absolute time
		case c.min&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// forward is next when it lies after t. A wall-clock time that falls in a daylight-saving gap or repeat can resolve
// to an instant at or before t; then the search moves on by an hour instead, so it always advances.
func forward(t, next time.Time) time.Time {
	if next.After(t) {
		return next
	}
	return t.Add(time.Hour)
}

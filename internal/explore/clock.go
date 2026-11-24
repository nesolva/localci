package explore

import "time"

// Clock is the time the schedule and the agent's time limit follow; tests inject their own.
type Clock interface {
	Now() time.Time
	// After sends once, d from now.
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// SystemClock is the real time.
var SystemClock Clock = systemClock{}

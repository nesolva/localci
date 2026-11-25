package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

// Sender is what Watch notifies through; *Notifier is one.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Watch notifies when a run finishes in a state listed in on ("failed", "passed"), until ctx ends. It follows the
// event log from now on, so runs that ended before the server started are not announced again. Only runs the
// person started by hand (`localci run`, a retry) notify: pull request runs show in the menu bar and the dashboard
// without a banner for every push.
func Watch(ctx context.Context, st *store.Store, bus *event.Bus, on []string, s Sender, url func(run string) string, logf func(string, ...any)) {
	if len(on) == 0 {
		return
	}
	since, err := st.LastSeq(ctx)
	if err != nil {
		logf("notifications are off: %v", err)
		return
	}
	for ev := range event.Subscribe(ctx, st, bus, since) {
		if ev.Kind != "run.state" || ev.Run == "" {
			continue
		}
		var d struct{ State string }
		if json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		outcome := Outcome(d.State)
		if outcome == "" || !slices.Contains(on, outcome) {
			continue
		}
		run, err := st.Run(ctx, ev.Run)
		if err != nil || !startedByHand(run) {
			continue
		}
		c, _ := st.TestCounts(ctx, run.ID) // the counts the dashboard and the pull request status show
		m := Describe(run, c.Failed, c.Total)
		if url != nil {
			m.URL = url(run.ID)
		}
		if err := s.Send(ctx, m); err != nil {
			logf("notification for run %s not shown: %v", run.ID, err)
		}
	}
}

func startedByHand(r store.Run) bool { return r.Trigger == "manual" || r.Trigger == "retry" }

// Outcome maps a run state to the name `notify.on` uses, "" for states that never notify (cancelled, running…).
func Outcome(state string) string {
	switch state {
	case store.StateFailed:
		return "failed"
	case store.StatePassed, store.StateReused:
		return "passed"
	}
	return ""
}

// Describe is the banner for a finished run: failed and total are the run's test counts (0 for a check).
func Describe(r store.Run, failed, total int) Message {
	outcome := Outcome(r.State)
	m := Message{Title: fmt.Sprintf("%s %s", r.Suite, outcome)}
	parts := []string{"#" + r.ID}
	switch {
	case r.Infra != "":
		m.Title = r.Suite + " could not run"
		parts = append(parts, r.Infra)
	case outcome == "failed" && failed > 0 && total > 0:
		parts = append(parts, fmt.Sprintf("%d of %d tests failed", failed, total))
	case outcome == "passed" && total > 0:
		parts = append(parts, fmt.Sprintf("%d tests", total))
	}
	if r.Started > 0 && r.Finished >= r.Started {
		parts = append(parts, took(time.Duration(r.Finished-r.Started)*time.Millisecond))
	}
	m.Body = strings.Join(parts, " · ")
	return m
}

func took(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

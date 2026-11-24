package web

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/nesolva/localci/internal/store"
)

// A step as the dashboard draws it. Start is absolute (unix ms); Open means the step never ended and nothing is
// running it any more (its run finished, or a later attempt of the test replaced it), so it is drawn unfinished
// instead of live.
type tstep struct {
	store.TimelineStep
	Start int64
	Open  bool
}

type lane struct {
	Shard int
	Steps []tstep
}

// flowInst is one call of a flow, from its flow.begin and flow.end events.
type flowInst struct {
	Step       string
	Flow       string
	Key        string
	Inputs     map[string]any
	Checkpoint string // saved | restored | none
	MS         int64
	Ended      bool
	Parent     string // the step that called it: the enclosing flow's step, or "" at the top of the test
}

// attempt is one try of one test, rebuilt from the run's events. The steps table keeps one row per step id, and a
// retry reuses the ids of the first try, so only the event log can tell the attempts apart.
type attempt struct {
	Test  string
	N     int // 0-based
	Shard int
	Steps []*tstep
	Flows []*flowInst
	steps map[string]*tstep
	flows map[string]*flowInst
}

// runData is every attempt of every test of a run.
type runData struct {
	attempts map[string][]*attempt
	order    []string // tests in the order they first began
}

func testOfStep(test, step string) string {
	if test != "" {
		return test
	}
	if k := strings.LastIndex(step, ":"); k > 0 {
		return step[:k]
	}
	return ""
}

// parseRunEvents folds a run's events (oldest first) into attempts. Only the core's own view of the report is
// used: events that were not valid protocol lines are stored under another kind and never reach this far.
func parseRunEvents(evs []store.Event) *runData {
	d := &runData{attempts: map[string][]*attempt{}}
	cur := func(test string, shard int) *attempt {
		as := d.attempts[test]
		if len(as) == 0 {
			d.order = append(d.order, test)
			as = []*attempt{newAttempt(test, 0, shard)}
			d.attempts[test] = as
		}
		return as[len(as)-1]
	}
	for _, e := range evs {
		switch e.Kind {
		case "test.begin":
			if e.Test == "" {
				continue
			}
			as := d.attempts[e.Test]
			if len(as) == 0 {
				d.order = append(d.order, e.Test)
			}
			d.attempts[e.Test] = append(as, newAttempt(e.Test, len(as), e.Shard))
		case "step.begin":
			var p struct {
				TS     int64   `json:"ts"`
				Test   string  `json:"test"`
				Step   string  `json:"step"`
				Parent *string `json:"parent"`
				Title  string  `json:"title"`
				Kind   string  `json:"kind"`
			}
			if json.Unmarshal(e.Data, &p) != nil || p.Step == "" {
				continue
			}
			test := testOfStep(e.Test, p.Step)
			if test == "" {
				test = p.Test
			}
			a := cur(test, e.Shard)
			ts := p.TS
			if ts == 0 {
				ts = e.TS
			}
			st := &tstep{TimelineStep: store.TimelineStep{Test: test, ID: p.Step, Title: p.Title, Kind: p.Kind}, Start: ts}
			if p.Parent != nil {
				st.Parent = *p.Parent
			}
			a.steps[p.Step] = st
			a.Steps = append(a.Steps, st)
		case "step.end":
			var p struct {
				Step string  `json:"step"`
				MS   float64 `json:"ms"`
				OK   bool    `json:"ok"`
			}
			if json.Unmarshal(e.Data, &p) != nil || p.Step == "" {
				continue
			}
			a := cur(testOfStep(e.Test, p.Step), e.Shard)
			if st := a.steps[p.Step]; st != nil {
				ok := p.OK
				st.MS, st.OK = int64(p.MS+.5), &ok
			}
		case "flow.begin":
			var p struct {
				Test   string         `json:"test"`
				Step   string         `json:"step"`
				Flow   string         `json:"flow"`
				Inputs map[string]any `json:"inputs"`
				Key    string         `json:"key"`
			}
			if json.Unmarshal(e.Data, &p) != nil || p.Step == "" || p.Flow == "" || !store.ValidKind(p.Key) {
				continue
			}
			test := testOfStep(e.Test, p.Step)
			if test == "" {
				test = p.Test
			}
			a := cur(test, e.Shard)
			f := &flowInst{Step: p.Step, Flow: p.Flow, Key: p.Key, Inputs: p.Inputs}
			if st := a.steps[p.Step]; st != nil {
				f.Parent = st.Parent
			}
			a.flows[p.Step] = f
			a.Flows = append(a.Flows, f)
		case "flow.end":
			var p struct {
				Step       string  `json:"step"`
				Checkpoint string  `json:"checkpoint"`
				MS         float64 `json:"ms"`
			}
			if json.Unmarshal(e.Data, &p) != nil || p.Step == "" {
				continue
			}
			a := cur(testOfStep(e.Test, p.Step), e.Shard)
			if f := a.flows[p.Step]; f != nil {
				f.Checkpoint, f.MS, f.Ended = p.Checkpoint, int64(p.MS+.5), true
			}
		}
	}
	return d
}

func newAttempt(test string, n, shard int) *attempt {
	return &attempt{Test: test, N: n, Shard: shard, steps: map[string]*tstep{}, flows: map[string]*flowInst{}}
}

// pick is the attempt of a test to show: sel is a 0-based attempt, or negative for the last one. A test with fewer
// attempts shows its last, so one test never mixes two attempts.
func (d *runData) pick(test string, sel int) *attempt {
	as := d.attempts[test]
	if len(as) == 0 {
		return nil
	}
	if sel < 0 || sel >= len(as) {
		return as[len(as)-1]
	}
	return as[sel]
}

func (d *runData) last(test string) *attempt { return d.pick(test, -1) }

// maxAttempts is the most attempts any test has, to know whether a selector is worth showing.
func (d *runData) maxAttempts() int {
	n := 0
	for _, as := range d.attempts {
		n = max(n, len(as))
	}
	return n
}

func (d *runData) hasSteps() bool {
	for _, as := range d.attempts {
		for _, a := range as {
			if len(a.Steps) > 0 {
				return true
			}
		}
	}
	return false
}

// timeLanes lays the chosen attempt of every test onto its shard's lane. Offsets are relative to the run's start,
// or to the earliest step when none is known (all attempts count, so choosing another attempt never moves the axis).
func (d *runData) timeLanes(sel int, active bool, runStarted int64) []lane {
	base := int64(0)
	for _, as := range d.attempts {
		for _, a := range as {
			for _, st := range a.Steps {
				if base == 0 || st.Start < base {
					base = st.Start
				}
			}
		}
	}
	if runStarted > 0 && (base == 0 || runStarted < base) {
		base = runStarted
	}
	byShard := map[int]*lane{}
	var shards []int
	for _, test := range d.order {
		a := d.pick(test, sel)
		if a == nil {
			continue
		}
		latest := a == d.last(test)
		for _, st := range a.Steps {
			c := *st
			c.Offset = st.Start - base
			c.Open = st.OK == nil && (!active || !latest)
			l := byShard[a.Shard]
			if l == nil {
				l = &lane{Shard: a.Shard}
				byShard[a.Shard] = l
				shards = append(shards, a.Shard)
			}
			l.Steps = append(l.Steps, c)
		}
	}
	sort.Ints(shards)
	out := make([]lane, 0, len(shards))
	for _, s := range shards {
		out = append(out, *byShard[s])
	}
	return out
}

// lanesFromStore is the fallback for a run whose steps were stored without events.
func lanesFromStore(ls []store.Lane, active bool) []lane {
	out := make([]lane, 0, len(ls))
	for _, l := range ls {
		o := lane{Shard: l.Shard, Steps: make([]tstep, 0, len(l.Steps))}
		for _, st := range l.Steps {
			o.Steps = append(o.Steps, tstep{TimelineStep: st, Open: st.OK == nil && !active})
		}
		out = append(out, o)
	}
	return out
}

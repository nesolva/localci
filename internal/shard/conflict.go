package shard

import (
	"cmp"
	"slices"
	"time"
)

// Result is one execution of one test.
type Result struct {
	ID     TestID
	Shard  int // shard it ran on; ignored for runs alone
	Passed bool
	Start  time.Time
	End    time.Time
}

// Conflict is a test that fails only next to others.
type Conflict struct {
	ID       TestID
	Evidence []TestID // tests on other shards that were running while it failed, longest overlap first
}

// alonePasses is how many clean runs alone prove a failure was not the test's own.
const alonePasses = 2

// Detect finds the tests that fail in parallel but pass alone twice and never
// fail alone: they depend on the shared state of whatever ran beside them.
// The evidence is the tests on other shards whose time window overlapped the
// failure; a failure with nothing overlapping it is a flake, not a conflict.
// A test that fails alone too is simply broken and never reported here.
func Detect(parallel, alone []Result) []Conflict {
	type tally struct{ pass, fail int }
	solo := map[TestID]*tally{}
	for _, r := range alone {
		t := solo[r.ID]
		if t == nil {
			t = new(tally)
			solo[r.ID] = t
		}
		if r.Passed {
			t.pass++
		} else {
			t.fail++
		}
	}

	failed := map[TestID][]Result{}
	passed := map[TestID]bool{}
	var order []TestID
	for _, r := range parallel {
		if r.Passed {
			passed[r.ID] = true
			continue
		}
		if len(failed[r.ID]) == 0 {
			order = append(order, r.ID)
		}
		failed[r.ID] = append(failed[r.ID], r)
	}

	var out []Conflict
	for _, id := range order {
		t := solo[id]
		if passed[id] || t == nil || t.fail > 0 || t.pass < alonePasses {
			continue
		}
		overlap := map[TestID]time.Duration{}
		for _, f := range failed[id] {
			for _, o := range parallel {
				if o.ID == id || o.Shard == f.Shard {
					continue
				}
				if d := earlier(f.End, o.End).Sub(later(f.Start, o.Start)); d > 0 {
					overlap[o.ID] += d
				}
			}
		}
		if len(overlap) == 0 {
			continue
		}
		ev := make([]TestID, 0, len(overlap))
		for o := range overlap {
			ev = append(ev, o)
		}
		slices.SortFunc(ev, func(a, b TestID) int {
			if c := cmp.Compare(overlap[b], overlap[a]); c != 0 {
				return c
			}
			return compareIDs(a, b)
		})
		out = append(out, Conflict{ID: id, Evidence: ev})
	}
	return out
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

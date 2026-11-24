package shard

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// SharedName is the one database the shared-db shards of a run use: the name of shard 0 with its last character
// swapped for s, so it has the same length and hash.
func SharedName(instance, suite, run string) string {
	n := ShardName(instance, suite, run, 0)
	return n[:len(n)-1] + "s"
}

// AccessConflict reports whether two tests cannot run at the same time in one database: one writes a table the other
// reads or writes.
func AccessConflict(a, b Access) bool {
	return overlaps(a.Writes, b.Reads) || overlaps(a.Writes, b.Writes) || overlaps(b.Writes, a.Reads)
}

func overlaps(x, y []string) bool {
	for _, t := range x {
		if slices.Contains(y, t) {
			return true
		}
	}
	return false
}

// Components groups tests that conflict directly or through others (a table with a writer joins its writers and
// readers), in a stable order. Tests in different components never conflict.
func Components(tests []TestID, known map[TestID]Access) [][]TestID {
	ids := slices.Clone(tests)
	slices.Sort(ids)
	parent := make([]int, len(ids))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[max(ra, rb)] = min(ra, rb)
		}
	}
	writer := map[string]int{} // table -> the first test that writes it
	for i, id := range ids {
		for _, t := range known[id].Writes {
			if w, ok := writer[t]; ok {
				union(w, i)
			} else {
				writer[t] = i
			}
		}
	}
	for i, id := range ids {
		for _, t := range known[id].Reads {
			if w, ok := writer[t]; ok {
				union(w, i)
			}
		}
	}
	byRoot := map[int][]TestID{}
	var roots []int
	for i, id := range ids {
		r := find(i)
		if _, ok := byRoot[r]; !ok {
			roots = append(roots, r)
		}
		byRoot[r] = append(byRoot[r], id)
	}
	slices.Sort(roots)
	out := make([][]TestID, len(roots))
	for i, r := range roots {
		out[i] = byRoot[r]
	}
	return out
}

// SharedLayout is a run split over shared-db shards, isolated shards and the serial group.
type SharedLayout struct {
	Shared   [][]TestID // shards that use the one shared database
	Isolated [][]TestID // shards with a database of their own (tests whose write set is unknown)
	Serial   []TestID
	Makespan time.Duration
	Alone    time.Duration
}

// PlanShared places the tests of a run on n shards. Tests with a known access set are grouped in conflict
// components and each component goes whole to one shared-db shard, so conflicting tests never run at once. Tests
// with an unknown set get isolated shards, their share of n proportional to their duration (at least one shard
// each side; with a single shard everything is isolated). Serial tests stay out of the shards.
func PlanShared(tests []TestID, medians map[TestID]time.Duration, known map[TestID]Access, n int, serial map[TestID]bool) SharedLayout {
	fill := median(tests, medians)
	dur := func(id TestID) time.Duration {
		if d, ok := medians[id]; ok && d > 0 {
			return d
		}
		return fill
	}
	var knownT, unknownT, alone []TestID
	for _, id := range tests {
		_, isKnown := known[id]
		switch {
		case serial[id]:
			alone = append(alone, id)
		case isKnown:
			knownT = append(knownT, id)
		default:
			unknownT = append(unknownT, id)
		}
	}
	if n < 2 && len(unknownT) > 0 {
		unknownT, knownT = append(unknownT, knownT...), nil
	}
	l := SharedLayout{}
	for _, id := range alone {
		l.Alone += dur(id)
	}
	slices.SortFunc(alone, func(a, b TestID) int { return cmp.Or(cmp.Compare(dur(b), dur(a)), cmp.Compare(a, b)) })
	l.Serial = alone

	nShared, nIso := n, n
	if len(knownT) > 0 && len(unknownT) > 0 {
		var dk, du time.Duration
		for _, id := range knownT {
			dk += dur(id)
		}
		for _, id := range unknownT {
			du += dur(id)
		}
		nIso = min(max(int(float64(n)*float64(du)/float64(dk+du)+0.5), 1), n-1)
		nShared = n - nIso
	}

	if len(knownT) > 0 {
		comps := Components(knownT, known)
		pseudo := make([]TestID, len(comps))
		pd := map[TestID]time.Duration{}
		members := map[TestID][]TestID{}
		for i, c := range comps {
			id := TestID(fmt.Sprintf("c%06d", i))
			pseudo[i] = id
			for _, m := range c {
				pd[id] += dur(m)
			}
			slices.SortFunc(c, func(a, b TestID) int { return cmp.Or(cmp.Compare(dur(b), dur(a)), cmp.Compare(a, b)) })
			members[id] = c
		}
		p := Plan(pseudo, pd, nShared, nil)
		for _, s := range p.Shards {
			var tests []TestID
			for _, id := range s {
				tests = append(tests, members[id]...)
			}
			l.Shared = append(l.Shared, tests)
		}
		l.Makespan = p.Makespan
	}
	if len(unknownT) > 0 {
		p := Plan(unknownT, medians, nIso, nil)
		l.Isolated = p.Shards
		l.Makespan = max(l.Makespan, p.Makespan)
	}
	return l
}

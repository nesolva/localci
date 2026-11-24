// Package shard splits a suite into shards that finish together, gives each
// shard its own database cloned from a template, and finds tests that only
// fail when they run next to others.
package shard

import (
	"cmp"
	"slices"
	"time"

	"github.com/nesolva/localci/internal/capacity"
)

// TestID identifies a test across runs (hash of project, file and title path).
type TestID string

// defaultDuration stands in for every test when nothing is known about any.
const defaultDuration = 10 * time.Second

// Layout is the split of one suite run.
type Layout struct {
	Shards   [][]TestID    // one list per shard, in execution order; never empty
	Serial   []TestID      // tests that run alone after the parallel part
	Makespan time.Duration // estimated time of the slowest shard
	Alone    time.Duration // estimated time of the serial group
}

// Plan places tests on n shards, longest first onto the least loaded shard
// (LPT), so the shards end at about the same time. Tests in serial are kept
// out of the shards. A test without a known duration counts as the median of
// the known ones (10 s when none is known). The result depends only on the
// input values, never on map order.
func Plan(tests []TestID, medians map[TestID]time.Duration, n int, serial map[TestID]bool) Layout {
	fill := median(tests, medians)
	dur := func(id TestID) time.Duration {
		if d, ok := medians[id]; ok && d > 0 {
			return d
		}
		return fill
	}

	var parallel, alone []TestID
	for _, id := range tests {
		if serial[id] {
			alone = append(alone, id)
		} else {
			parallel = append(parallel, id)
		}
	}
	byLongest := func(a, b TestID) int {
		if c := cmp.Compare(dur(b), dur(a)); c != 0 {
			return c
		}
		return compareIDs(a, b)
	}
	slices.SortFunc(parallel, byLongest)
	slices.SortFunc(alone, byLongest)

	n = max(min(n, len(parallel)), 1)
	load := make([]time.Duration, n)
	shards := make([][]TestID, n)
	for _, id := range parallel {
		i := 0
		for j := range load {
			if load[j] < load[i] {
				i = j
			}
		}
		shards[i] = append(shards[i], id)
		load[i] += dur(id)
	}
	if len(parallel) == 0 {
		shards = nil
	}

	p := Layout{Shards: shards, Serial: alone}
	for _, l := range load {
		p.Makespan = max(p.Makespan, l)
	}
	for _, id := range alone {
		p.Alone += dur(id)
	}
	return p
}

func compareIDs(a, b TestID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// median of the known durations of tests; the default when there are none.
func median(tests []TestID, medians map[TestID]time.Duration) time.Duration {
	var known []time.Duration
	for _, id := range tests {
		if d := medians[id]; d > 0 {
			known = append(known, d)
		}
	}
	if len(known) == 0 {
		return defaultDuration
	}
	slices.Sort(known)
	mid := len(known) / 2
	if len(known)%2 == 1 {
		return known[mid]
	}
	return (known[mid-1] + known[mid]) / 2
}

// Cost is what one shard needs: its CPUs and memory, stack included.
type Cost struct {
	CPUs   int
	Memory uint64 // bytes
}

// DefaultCost is used until a shard's real cost has been measured.
var DefaultCost = Cost{CPUs: 1, Memory: 3 << 29} // 1 CPU, 1.5 GiB

// Count is the shard number for `shards: auto`: as many shards as the machine
// can feed (CPUs, free memory and Docker VM memory, each divided by the cost
// of one shard; an unknown budget does not limit), never more than tests and
// never fewer than one. Zero cost fields fall back to DefaultCost.
func Count(m capacity.Machine, perShard Cost, tests int) int {
	if perShard.CPUs <= 0 {
		perShard.CPUs = DefaultCost.CPUs
	}
	if perShard.Memory == 0 {
		perShard.Memory = DefaultCost.Memory
	}
	n := max(tests, 1)
	if m.CPUs > 0 {
		n = min(n, m.CPUs/perShard.CPUs)
	}
	if m.FreeMemory > 0 {
		n = min(n, int(m.FreeMemory/perShard.Memory))
	}
	if m.DockerMemory > 0 {
		n = min(n, int(m.DockerMemory/perShard.Memory))
	}
	return max(n, 1)
}

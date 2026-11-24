package shard

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/capacity"
)

func randomSuite(r *rand.Rand, size int) ([]TestID, map[TestID]time.Duration) {
	ids := make([]TestID, size)
	med := map[TestID]time.Duration{}
	for i := range ids {
		ids[i] = TestID(fmt.Sprintf("t%03d", i))
		med[ids[i]] = time.Duration(1+r.Intn(60_000)) * time.Millisecond
	}
	return ids, med
}

func TestPlanMakespanNearLowerBound(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for range 200 {
		n := 2 + r.Intn(7)
		ids, med := randomSuite(r, 30+r.Intn(170))
		p := Plan(ids, med, n, nil)

		var total, longest time.Duration
		for _, d := range med {
			total += d
			longest = max(longest, d)
		}
		bound := max(longest, total/time.Duration(n))
		if p.Makespan > bound*110/100 {
			t.Fatalf("n=%d tests=%d: makespan %v exceeds 110%% of lower bound %v", n, len(ids), p.Makespan, bound)
		}
	}
}

func TestPlanDeterministicAndComplete(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	ids, med := randomSuite(r, 80)
	a := Plan(ids, med, 5, nil)
	b := Plan(slices.Clone(ids), med, 5, nil)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same input gave different plans")
	}
	var seen []TestID
	for _, s := range a.Shards {
		seen = append(seen, s...)
	}
	slices.Sort(seen)
	want := slices.Clone(ids)
	slices.Sort(want)
	if !slices.Equal(seen, want) {
		t.Fatal("every test must be in exactly one shard")
	}
}

func TestPlanSerialNeverInShards(t *testing.T) {
	ids := []TestID{"a", "b", "c", "d", "e"}
	med := map[TestID]time.Duration{"a": 9 * time.Second, "b": 8 * time.Second, "c": 7 * time.Second, "d": 6 * time.Second, "e": 5 * time.Second}
	serial := map[TestID]bool{"a": true, "d": true}
	p := Plan(ids, med, 3, serial)
	for _, s := range p.Shards {
		for _, id := range s {
			if serial[id] {
				t.Fatalf("%s is serial but sits in a shard", id)
			}
		}
	}
	if !slices.Equal(p.Serial, []TestID{"a", "d"}) {
		t.Fatalf("serial = %v", p.Serial)
	}
	if p.Alone != 15*time.Second {
		t.Fatalf("alone time = %v", p.Alone)
	}
}

func TestPlanUnknownDurationsUseMedianOfKnown(t *testing.T) {
	// known: 2s and 4s -> median 3s; the three unknown tests count 3s each: 15s over
	// two shards, best split 8s + 7s.
	ids := []TestID{"k2", "k4", "u1", "u2", "u3"}
	med := map[TestID]time.Duration{"k2": 2 * time.Second, "k4": 4 * time.Second}
	if got := Plan(ids, med, 2, nil).Makespan; got != 8*time.Second {
		t.Fatalf("makespan = %v, want 8s", got)
	}
	// With no history at all every test counts 10s.
	if got := Plan([]TestID{"a", "b", "c"}, nil, 3, nil).Makespan; got != 10*time.Second {
		t.Fatalf("makespan without history = %v, want 10s", got)
	}
}

func TestPlanNeverMoreShardsThanTests(t *testing.T) {
	p := Plan([]TestID{"a", "b"}, nil, 8, nil)
	if len(p.Shards) != 2 {
		t.Fatalf("got %d shards for 2 tests", len(p.Shards))
	}
}

func TestCount(t *testing.T) {
	const gib = uint64(1) << 30
	m := capacity.Machine{CPUs: 8, FreeMemory: 12 * gib, DockerMemory: 6 * gib}
	cases := []struct {
		name  string
		m     capacity.Machine
		cost  Cost
		tests int
		want  int
	}{
		{"docker memory is the limit", m, Cost{}, 100, 4},
		{"measured cost", m, Cost{CPUs: 2, Memory: 1 * gib}, 100, 4},
		{"never more than tests", m, Cost{}, 2, 2},
		{"unknown memory does not limit", capacity.Machine{CPUs: 4}, Cost{}, 100, 4},
		{"at least one", capacity.Machine{CPUs: 1, FreeMemory: gib}, Cost{}, 100, 1},
	}
	for _, c := range cases {
		if got := Count(c.m, c.cost, c.tests); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

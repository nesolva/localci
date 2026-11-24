package shard

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"
)

func randomAccess(r *rand.Rand, tables int) Access {
	pick := func(n int) []string {
		m := map[string]bool{}
		for range n {
			m[fmt.Sprintf("t%d", r.Intn(tables))] = true
		}
		return sortedKeys(m)
	}
	w := pick(r.Intn(3))
	return Access{Writes: w, Reads: Access{}.Union(Access{Writes: w, Reads: pick(r.Intn(3))}).Reads}
}

// Conflicting tests never share a window: whatever the sets, no two tests that conflict land on different
// shared shards, every test is placed exactly once, and tests with no overlap still spread over the shards.
func TestPlanSharedNeverSplitsConflictingTests(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		count := 2 + r.Intn(40)
		known := map[TestID]Access{}
		var ids []TestID
		medians := map[TestID]time.Duration{}
		for i := range count {
			id := TestID(fmt.Sprintf("test%02d", i))
			ids = append(ids, id)
			medians[id] = time.Duration(1+r.Intn(20)) * time.Second
			if r.Intn(5) > 0 { // a few unknown
				known[id] = randomAccess(r, 3+r.Intn(12))
			}
		}
		serial := map[TestID]bool{ids[0]: r.Intn(4) == 0}
		n := 1 + r.Intn(6)
		l := PlanShared(ids, medians, known, n, serial)

		shardOf := map[TestID]int{}
		placed := map[TestID]int{}
		for i, s := range l.Shared {
			for _, id := range s {
				shardOf[id] = i
				placed[id]++
				if _, ok := known[id]; !ok {
					t.Fatalf("seed %d: %s has an unknown set but sits in the shared database", seed, id)
				}
			}
		}
		for _, s := range l.Isolated {
			for _, id := range s {
				placed[id]++
			}
		}
		for _, id := range l.Serial {
			placed[id]++
		}
		for _, id := range ids {
			if placed[id] != 1 {
				t.Fatalf("seed %d: %s placed %d times", seed, id, placed[id])
			}
		}
		for _, a := range ids {
			for _, b := range ids {
				sa, oka := shardOf[a]
				sb, okb := shardOf[b]
				if a < b && oka && okb && sa != sb && AccessConflict(known[a], known[b]) {
					t.Fatalf("seed %d: %s and %s conflict but run on shared shards %d and %d", seed, a, b, sa, sb)
				}
			}
		}
		if len(l.Shared)+len(l.Isolated) > max(n, 1) {
			t.Fatalf("seed %d: %d shards for n=%d", seed, len(l.Shared)+len(l.Isolated), n)
		}
	}
}

func TestPlanSharedSpreadsDisjointTestsAndIsolatesUnknown(t *testing.T) {
	ids := []TestID{"a", "b", "c", "d", "u"}
	known := map[TestID]Access{
		"a": {Writes: []string{"x"}, Reads: []string{"x"}},
		"b": {Writes: []string{"y"}, Reads: []string{"y"}},
		"c": {Writes: []string{"z"}, Reads: []string{"z"}},
		"d": {}, // measured, touches nothing: known
	}
	l := PlanShared(ids, nil, known, 4, nil)
	if len(l.Isolated) != 1 || !slices.Equal(l.Isolated[0], []TestID{"u"}) {
		t.Fatalf("the unknown test must get an isolated shard: %v", l.Isolated)
	}
	if len(l.Shared) != 3 {
		t.Fatalf("four disjoint tests on three shared shards, got %v", l.Shared)
	}
	if l := PlanShared(ids, nil, known, 1, nil); len(l.Shared) != 0 || len(l.Isolated) != 1 {
		t.Fatalf("one shard with unknown tests means everything isolated: %+v", l)
	}
}

func TestSharedNameStartsWithPrefixAndFitsTheLimit(t *testing.T) {
	long := strings.Repeat("web-e2e-", 20)
	for _, suite := range []string{"a", "web", long} {
		name := SharedName(testInstance, suite, strings.Repeat("9", 40))
		if !strings.HasPrefix(name, "localci_") || len(name) > 63 || !strings.HasSuffix(name, "_s") {
			t.Fatalf("%q (%d bytes)", name, len(name))
		}
		if name == ShardName(testInstance, suite, strings.Repeat("9", 40), 0) || checkDroppable(name) != nil {
			t.Fatalf("%q must differ from shard 0 and be droppable", name)
		}
	}
}

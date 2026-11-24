package memo

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

func pre(sigs ...string) []string { return sigs }

func newPlan(shards int, prefixes map[string][]string) *Planner {
	var ids []string
	for id := range prefixes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return NewPlanner(prefixes, ids, shards)
}

// Two sibling subtrees share the first node. The second subtree's owner may not start until the shared node is
// built, and then it builds only what is its own. An owner takes only the first test of its group.
func TestASecondOwnerWaitsForTheSharedNodeThenBuildsOnlyItsOwn(t *testing.T) {
	p := newPlan(3, map[string][]string{
		"a1": pre("login", "order-a"), "a2": pre("login", "order-a"),
		"b1": pre("login", "order-b"), "b2": pre("login", "order-b"),
		"free": nil,
	})
	if !p.Shared() {
		t.Fatal("a tree with a shared node reported nothing to share")
	}
	w1 := p.Try(1)
	if !slices.Equal(w1.Tests, []string{"a1"}) {
		t.Fatalf("first shard got %v, want the first test of the first subtree", w1.Tests)
	}
	// A test without a prefix is ready at once: a shard that cannot own anything yet does not idle.
	if w2 := p.Try(2); !slices.Equal(w2.Tests, []string{"free"}) {
		t.Fatalf("second shard got %v, want the test without a prefix", w2.Tests)
	}
	if w3 := p.Try(3); !w3.Wait || len(w3.Tests) != 0 {
		t.Fatalf("third shard got %+v while the shared node is being built, want wait", w3)
	}
	p.FlowEnd("a1", 0, "login") // the owner finished the shared flow
	if w3 := p.Try(3); !slices.Equal(w3.Tests, []string{"b1"}) {
		t.Fatalf("third shard got %+v once login is done, want the first test of the second subtree", w3)
	}
}

// The end of the owner's batch is enough to free the tests under it, even when no flow end was ever seen: a
// crashed owner never leaves followers waiting.
func TestTheEndOfAnOwnersBatchReleasesItsFollowers(t *testing.T) {
	p := newPlan(2, map[string][]string{
		"a": pre("login", "order-a"), "b": pre("login", "order-b"),
	})
	if w := p.Try(1); !slices.Equal(w.Tests, []string{"a"}) {
		t.Fatalf("owner got %v", w.Tests)
	}
	if w := p.Try(2); !w.Wait {
		t.Fatalf("follower got %+v before the owner is done, want wait", w)
	}
	p.Release(1) // the owner's batch ended without reporting a flow
	if w := p.Try(2); !slices.Equal(w.Tests, []string{"b"}) {
		t.Fatalf("follower got %+v after the owner's batch ended, want its test", w)
	}
}

// Whatever order shards finish in, every test is handed out exactly once, a test is never handed out under a
// node another shard is still building, and the planner ends.
func TestEveryTestIsHandedOutOnceAndNeverUnderAnUnbuiltNode(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		rng := rand.New(rand.NewSource(seed))
		prefixes := map[string][]string{}
		var ids []string
		for i := range 30 {
			id := string(rune('a'+i%26)) + string(rune('0'+i/26))
			ids = append(ids, id)
			switch rng.Intn(4) {
			case 0:
			case 1:
				prefixes[id] = pre("login")
			default:
				prefixes[id] = pre("login", "order-"+string(rune('a'+rng.Intn(4))))
			}
		}
		const shards = 4
		p := NewPlanner(prefixes, ids, shards)
		handed := map[string]int{}
		built := map[string]bool{} // node path -> built
		running := map[int][]string{}
		for steps := 0; steps < 1000; steps++ {
			s := 1 + rng.Intn(shards)
			if batch, ok := running[s]; ok {
				// The shard finishes: it reports the shared flows of the first test it ran, then asks again.
				if len(batch) > 0 {
					for i, sig := range prefixes[batch[0]] {
						p.FlowEnd(batch[0], i, sig)
						built[key(prefixes[batch[0]][:i+1])] = true
					}
				}
				delete(running, s)
			}
			w := p.Try(s)
			if w.Done {
				continue
			}
			if w.Wait {
				continue
			}
			for _, id := range w.Tests {
				handed[id]++
				for i := range prefixes[id] {
					// Every node under the test is built, or is built by this very batch.
					if !built[key(prefixes[id][:i+1])] && !ownedBy(prefixes, w.Tests, key(prefixes[id][:i+1])) {
						t.Fatalf("seed %d: %s handed out under an unbuilt node %v", seed, id, prefixes[id][:i+1])
					}
				}
			}
			running[s] = w.Tests
		}
		for _, id := range ids {
			if handed[id] != 1 {
				t.Fatalf("seed %d: %s handed out %d times", seed, id, handed[id])
			}
		}
		if left := p.Left(); len(left) != 0 {
			t.Fatalf("seed %d: %v left", seed, left)
		}
	}
}

func key(sigs []string) string { return join(sigs) }

func ownedBy(prefixes map[string][]string, batch []string, node string) bool {
	for _, id := range batch {
		for i := range prefixes[id] {
			if key(prefixes[id][:i+1]) == node {
				return true
			}
		}
	}
	return false
}

// A shard asking for work with nothing ready blocks until a node completes, and stops with the context.
func TestNextBlocksUntilWorkIsReadyAndStopsWithItsContext(t *testing.T) {
	p := newPlan(2, map[string][]string{"a": pre("login", "x"), "b": pre("login", "y")})
	p.Try(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan Work, 1)
	go func() {
		w, _ := p.Next(ctx, 2)
		got <- w
	}()
	select {
	case w := <-got:
		t.Fatalf("Next returned %+v before the shared node was built", w)
	case <-time.After(100 * time.Millisecond):
	}
	p.FlowEnd("a", 0, "login")
	select {
	case w := <-got:
		if !slices.Equal(w.Tests, []string{"b"}) {
			t.Fatalf("Next returned %+v, want b", w)
		}
	case <-time.After(time.Second):
		t.Fatal("Next did not wake when the node was built")
	}
	// A shard stuck waiting stops with its context.
	q := newPlan(2, map[string][]string{"a": pre("login", "x"), "b": pre("login", "y")})
	q.Try(1)
	cctx, ccancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer ccancel()
	if _, err := q.Next(cctx, 2); err == nil {
		t.Fatal("a waiting shard did not stop with its context")
	}
}

// Without anything shared there is nothing to schedule.
func TestNothingSharedMeansNoPlan(t *testing.T) {
	if newPlan(2, map[string][]string{"a": pre("x"), "b": pre("y"), "c": nil}).Shared() {
		t.Fatal("distinct prefixes reported as shared")
	}
}

// One leaf with many tests under it: the owner takes only the first, so the others spread over the shards once the
// leaf is built instead of all running on the owner's shard.
func TestManyTestsUnderOneLeafAreSharedOutOverTheShardsOnceTheLeafIsBuilt(t *testing.T) {
	m := map[string][]string{}
	for i := 0; i < 20; i++ {
		m[fmt.Sprintf("t%02d", i)] = pre("login")
	}
	p := newPlan(4, m)
	w1 := p.Try(1)
	if len(w1.Tests) != 1 {
		t.Fatalf("the owner took %d tests, want only the first", len(w1.Tests))
	}
	if w2 := p.Try(2); !w2.Wait {
		t.Fatalf("shard 2 before the leaf is built: %+v", w2)
	}
	p.FlowEnd(w1.Tests[0], 0, "login")
	got := map[int]int{}
	for s := 2; s <= 4; s++ {
		w := p.Try(s)
		if w.Done || w.Wait || len(w.Tests) == 0 {
			t.Fatalf("shard %d after the leaf is built got %+v", s, w)
		}
		got[s] = len(w.Tests)
	}
	t.Logf("shares after the leaf: %v", got)
}

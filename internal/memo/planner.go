package memo

import (
	"context"
	"slices"
	"sync"
)

// Work is what a shard is told to do next. Tests is a batch to run as one invocation; Wait means nothing is ready
// yet (a shared node is being built) and the shard asks again; Done means this shard has nothing left to do.
type Work struct {
	Tests []string `json:"tests"`
	Wait  bool     `json:"wait"`
	Done  bool     `json:"done"`
}

type nodeState int

const (
	free nodeState = iota
	claimed
	built
)

// Planner hands tests to shards so that every node of the prefix tree is built by one shard and the tests under
// it restore from it. A shard that asks for work releases the batch it had: the end of a batch marks the nodes it
// claimed as built whether or not they were, so a crashed owner never leaves followers waiting (they just run the
// flows themselves, which is the unplanned behaviour).
type Planner struct {
	mu      sync.Mutex
	shards  int
	order   []string            // tests in the order they were given
	path    map[string][]string // test -> node ids from the root, one per prefix flow
	sigs    map[string][]string // test -> the signatures its path was built from
	pending map[string]bool
	state   map[string]nodeState
	batch   map[int]*batch // shard -> the batch it is running
	changed chan struct{}  // closed and replaced on every state change
}

type batch struct{ nodes []string }

// NewPlanner plans the given tests (a test without a learned prefix has no nodes and is always ready).
func NewPlanner(prefixes map[string][]string, tests []string, shards int) *Planner {
	p := &Planner{shards: max(shards, 1), order: slices.Clone(tests), path: map[string][]string{}, sigs: map[string][]string{},
		pending: map[string]bool{}, state: map[string]nodeState{}, batch: map[int]*batch{}, changed: make(chan struct{})}
	for _, id := range tests {
		p.pending[id] = true
		sigs := prefixes[id]
		nodes := make([]string, len(sigs))
		for i := range sigs {
			nodes[i] = join(sigs[:i+1])
		}
		p.path[id], p.sigs[id] = nodes, sigs
	}
	return p
}

// Shared says whether any node is under two or more tests: with none, scheduling cannot save anything.
func (p *Planner) Shared() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := map[string]int{}
	for _, id := range p.order {
		for _, n := range p.path[id] {
			if count[n]++; count[n] > 1 {
				return true
			}
		}
	}
	return false
}

func (p *Planner) signal() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// FlowEnd reports that test finished its index-th top-level flow with the given signature. When that is the node
// the test's path expects there, the node is built and the tests waiting for it can start.
func (p *Planner) FlowEnd(test string, index int, signature string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	nodes, sigs := p.path[test], p.sigs[test]
	if index < 0 || index >= len(nodes) || sigs[index] != signature {
		return
	}
	if p.state[nodes[index]] != built {
		p.state[nodes[index]] = built
		p.signal()
	}
}

// Release ends the shard's batch: the nodes it claimed count as built.
func (p *Planner) Release(shard int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.release(shard)
}

func (p *Planner) release(shard int) {
	b := p.batch[shard]
	if b == nil {
		return
	}
	delete(p.batch, shard)
	for _, n := range b.nodes {
		p.state[n] = built
	}
	p.signal()
}

// Left lists the tests never handed out.
func (p *Planner) Left() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, id := range p.order {
		if p.pending[id] {
			out = append(out, id)
		}
	}
	return out
}

// Try hands the shard its next batch, or says to wait or that it is done. It never blocks.
func (p *Planner) Try(shard int) Work {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.try(shard)
}

// Next is Try that waits while nothing is ready, and stops with ctx.
func (p *Planner) Next(ctx context.Context, shard int) (Work, error) {
	for {
		p.mu.Lock()
		w := p.try(shard)
		ch := p.changed
		p.mu.Unlock()
		if !w.Wait {
			return w, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return Work{Done: true}, ctx.Err()
		}
	}
}

func (p *Planner) try(shard int) Work {
	p.release(shard)
	// Owners first: the group of tests with the same leaf whose unbuilt nodes nobody is building. Its first test
	// builds the nodes it needs and the rest of the group waits for it; once they are built the rest goes through
	// the ready split below, so one shard does not take the whole group. The group that unlocks the most tests
	// starts first.
	if leaf, ok := p.owner(); ok {
		var tests []string
		for _, id := range p.order {
			if p.pending[id] && p.leaf(id) == leaf {
				tests = append(tests, id)
			}
		}
		b := &batch{}
		for _, n := range p.path[tests[0]] {
			if p.state[n] == free {
				p.state[n] = claimed
				b.nodes = append(b.nodes, n)
			}
		}
		return p.take(shard, tests[:1], b)
	}
	var ready []string
	for _, id := range p.order {
		if p.pending[id] && p.ready(id) {
			ready = append(ready, id)
		}
	}
	if len(ready) > 0 {
		n := (len(ready) + p.shards - 1) / p.shards
		return p.take(shard, ready[:n], &batch{})
	}
	var left []string
	for _, id := range p.order {
		if p.pending[id] {
			left = append(left, id)
		}
	}
	switch {
	case len(left) == 0:
		return Work{Done: true}
	case len(p.batch) == 0: // nothing is running, so nothing will ever become ready: run the rest unplanned
		return p.take(shard, left, &batch{})
	}
	return Work{Wait: true}
}

func (p *Planner) take(shard int, tests []string, b *batch) Work {
	for _, id := range tests {
		delete(p.pending, id)
	}
	p.batch[shard] = b
	p.signal()
	return Work{Tests: slices.Clone(tests)}
}

func (p *Planner) leaf(id string) string {
	n := p.path[id]
	if len(n) == 0 {
		return ""
	}
	return n[len(n)-1]
}

// ready: every node under the test is built.
func (p *Planner) ready(id string) bool {
	for _, n := range p.path[id] {
		if p.state[n] != built {
			return false
		}
	}
	return true
}

// owner picks the leaf of a pending group that has unbuilt nodes and none being built, preferring the one whose
// first unbuilt node has the most tests under it.
func (p *Planner) owner() (string, bool) {
	best, bestN := "", 0
	found := false
	seen := map[string]bool{}
	for _, id := range p.order {
		if !p.pending[id] || len(p.path[id]) == 0 {
			continue
		}
		leaf := p.leaf(id)
		if seen[leaf] {
			continue
		}
		seen[leaf] = true
		first, ok := "", true
		for _, n := range p.path[id] {
			switch p.state[n] {
			case claimed:
				ok = false
			case free:
				if first == "" {
					first = n
				}
			}
		}
		if !ok || first == "" {
			continue
		}
		under := 0
		for _, other := range p.order {
			if p.pending[other] && slices.Contains(p.path[other], first) {
				under++
			}
		}
		if !found || under > bestN {
			best, bestN, found = leaf, under, true
		}
	}
	return best, found
}

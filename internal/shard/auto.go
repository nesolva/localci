package shard

import "github.com/nesolva/localci/internal/capacity"

// Isolation levels a suite can use (see the roadmap's Contracts).
const (
	LevelIsolated = "isolated"
	LevelSharedDB = "shared-db"
)

// AutoInput is what `auto` weighs.
type AutoInput struct {
	Machine capacity.Machine
	Cost    Cost
	Tests   []TestID // the parallel tests (serial ones stay out)
	Known   map[TestID]Access
	// Trusted marks the tests whose set was last replaced by a run on every repository's default branch. Sharing a
	// database needs every test of the run trusted: a branch's run may only have added tables, so an old or partial
	// set would let two tests that conflict run together.
	Trusted  map[TestID]bool
	Measured bool // the engine can be measured (Postgres), or the suite reports its writes
	Fixed    int  // `shards: N` when set, else 0
	// Tenant says the suite declares a tenant flow and a stack, so tenant isolation is possible; it needs no
	// write sets.
	Tenant bool
}

// Choice is the level and shard count `auto` picked.
type Choice struct {
	Level  string
	Shards int
}

// Choose picks the level with the most shards that fit the machine among those whose preconditions hold; a tie
// goes to isolated, the safest. With isolated every shard has its own database, so a shard costs its stack and its
// database memory; with shared-db (only when every test's set is trusted) the database is not per shard, so the Docker VM's memory no longer limits the
// count, and there are never more shards than conflict components; with tenant the stack is paid once and write sets
// do not matter, so it needs only a declared tenant flow.
func Choose(in AutoInput) Choice {
	capTo := func(n int) int {
		if in.Fixed > 0 {
			n = min(n, in.Fixed)
		}
		return max(n, 1)
	}
	iso := Choice{Level: LevelIsolated, Shards: capTo(Count(in.Machine, in.Cost, len(in.Tests)))}
	if len(in.Tests) == 0 {
		return iso
	}
	best := iso
	if in.Measured && allTrusted(in) {
		m := in.Machine
		m.DockerMemory = 0
		ids := knownIDs(in)
		shared := Count(m, in.Cost, len(in.Tests))
		shared = capTo(min(shared, len(Components(ids, in.Known))+len(in.Tests)-len(ids)))
		if shared > best.Shards {
			best = Choice{Level: LevelSharedDB, Shards: shared}
		}
	}
	// Tenant wins only when it fits more shards than both of the others.
	if in.Tenant {
		if t := capTo(TenantCount(in.Machine, in.Cost, len(in.Tests))); t > best.Shards {
			best = Choice{Level: LevelTenant, Shards: t}
		}
	}
	return best
}

func knownIDs(in AutoInput) []TestID {
	var ids []TestID
	for _, id := range in.Tests {
		if _, ok := in.Known[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

func allTrusted(in AutoInput) bool {
	for _, id := range in.Tests {
		if _, ok := in.Known[id]; !ok || !in.Trusted[id] {
			return false
		}
	}
	return true
}

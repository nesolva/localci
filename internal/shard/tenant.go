package shard

import "github.com/nesolva/localci/internal/capacity"

// LevelTenant is the isolation level where every shard runs against one shared stack and one shared database,
// each in a tenant its own flow made (see the roadmap's Contracts).
const LevelTenant = "tenant"

// StackShare is the share of a shard's memory that belongs to the stack rather than to the test process, used
// until the two are measured apart: with tenant isolation the stack is paid once for the whole run.
const StackShare = 0.5

// TenantCount is how many shards fit the machine when they share one stack: the stack counts once, each shard
// only costs its test process. The database is not per shard, so the Docker VM's memory does not limit the
// count, and write sets do not either: tenants keep the shards apart.
func TenantCount(m capacity.Machine, perShard Cost, tests int) int {
	if perShard.Memory == 0 {
		perShard.Memory = DefaultCost.Memory
	}
	stack := uint64(float64(perShard.Memory) * StackShare)
	m.DockerMemory = 0
	if m.FreeMemory > 0 {
		if m.FreeMemory <= stack {
			return 1
		}
		m.FreeMemory -= stack
	}
	perShard.Memory -= stack
	return Count(m, perShard, tests)
}

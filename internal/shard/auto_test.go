package shard

import (
	"fmt"
	"testing"

	"github.com/nesolva/localci/internal/capacity"
)

func TestChooseLevel(t *testing.T) {
	mk := func(n, known, components int) ([]TestID, map[TestID]Access) {
		var ids []TestID
		k := map[TestID]Access{}
		for i := range n {
			id := TestID(fmt.Sprintf("t%02d", i))
			ids = append(ids, id)
			if i < known {
				tbl := fmt.Sprintf("x%d", i%components)
				k[id] = Access{Writes: []string{tbl}, Reads: []string{tbl}}
			}
		}
		return ids, k
	}
	cost := Cost{CPUs: 1, Memory: 1 << 30}
	// 8 CPUs, plenty of free memory, a Docker VM that fits only 2 shards.
	tight := capacity.Machine{CPUs: 8, FreeMemory: 32 << 30, DockerMemory: 2 << 30}
	roomy := capacity.Machine{CPUs: 8, FreeMemory: 32 << 30, DockerMemory: 32 << 30}
	cases := []struct {
		name              string
		m                 capacity.Machine
		tests, known, cmp int
		untrusted         int // known sets last replaced by a branch run
		measured          bool
		fixed             int
		want              Choice
	}{
		{"docker memory limits isolated, shared fits more", tight, 20, 20, 8, 0, true, 0, Choice{LevelSharedDB, 8}},
		{"tie goes to isolated", roomy, 20, 20, 8, 0, true, 0, Choice{LevelIsolated, 8}},
		{"one test without a set", tight, 20, 19, 8, 0, true, 0, Choice{LevelIsolated, 2}},
		{"one set not from the default branch", tight, 20, 20, 8, 1, true, 0, Choice{LevelIsolated, 2}},
		{"every set from a branch", tight, 20, 20, 8, 20, true, 0, Choice{LevelIsolated, 2}},
		{"engine cannot be measured", tight, 20, 20, 8, 0, false, 0, Choice{LevelIsolated, 2}},
		{"one conflict component caps the shards", tight, 20, 20, 1, 0, true, 0, Choice{LevelIsolated, 2}},
		{"shards: N caps both", tight, 20, 20, 8, 0, true, 4, Choice{LevelSharedDB, 4}},
		{"no tests", tight, 0, 0, 1, 0, true, 0, Choice{LevelIsolated, 1}},
	}
	for _, c := range cases {
		ids, known := mk(c.tests, c.known, max(c.cmp, 1))
		trusted := map[TestID]bool{}
		for i, id := range ids {
			trusted[id] = i < c.known-c.untrusted
		}
		got := Choose(AutoInput{Machine: c.m, Cost: cost, Tests: ids, Known: known, Trusted: trusted, Measured: c.measured, Fixed: c.fixed})
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Tenant is offered only with a declared tenant flow and is chosen only when it fits more shards than both
// isolated and shared-db.
func TestChooseTenant(t *testing.T) {
	var ids []TestID
	known := map[TestID]Access{}
	for i := range 20 {
		id := TestID(fmt.Sprintf("t%02d", i))
		ids = append(ids, id)
		known[id] = Access{Writes: []string{"orders"}, Reads: []string{"orders"}} // one conflict component
	}
	cost := Cost{CPUs: 1, Memory: 1 << 30}
	// Memory for 4 whole shards: 4 isolated, but 7 test processes beside one stack (4 GiB - 0.5 GiB) / 0.5 GiB.
	tight := capacity.Machine{CPUs: 16, FreeMemory: 4 << 30, DockerMemory: 32 << 30}
	cases := []struct {
		name     string
		tenant   bool
		measured bool
		fixed    int
		want     Choice
	}{
		{"no tenant flow stays isolated", false, true, 0, Choice{LevelIsolated, 4}},
		{"tenant flow fits more shards", true, true, 0, Choice{LevelTenant, 7}},
		{"tenant needs no write sets", true, false, 0, Choice{LevelTenant, 7}},
		{"shards: N caps tenant, then isolated is as good", true, true, 4, Choice{LevelIsolated, 4}},
	}
	for _, c := range cases {
		got := Choose(AutoInput{Machine: tight, Cost: cost, Tests: ids, Known: known, Measured: c.measured, Fixed: c.fixed, Tenant: c.tenant})
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	// Disjoint write sets: shared-db fits 16 shards, so tenant (7) does not win.
	trusted := map[TestID]bool{}
	for i, id := range ids {
		known[id] = Access{Writes: []string{fmt.Sprintf("x%d", i)}}
		trusted[id] = true
	}
	roomy := capacity.Machine{CPUs: 16, FreeMemory: 64 << 30, DockerMemory: 1 << 30}
	if got := Choose(AutoInput{Machine: roomy, Cost: cost, Tests: ids, Known: known, Trusted: trusted, Measured: true, Tenant: true}); got.Level == LevelTenant {
		t.Errorf("tenant chosen although shared-db fits as many shards: %+v", got)
	}
}

package shard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceIDIsStablePerDataDirAndDiffersBetweenThem(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	first := InstanceID(a)
	if first == "" {
		t.Fatal("empty instance id")
	}
	if again := InstanceID(a); again != first {
		t.Errorf("id changed between calls: %q then %q", first, again)
	}
	if other := InstanceID(b); other == first {
		t.Errorf("two data directories share the id %q", first)
	}
	// a restart reads what the first start stored
	if b, err := os.ReadFile(filepath.Join(a, "instance-id")); err != nil || len(b) == 0 {
		t.Errorf("id is not stored in the data directory: %v", err)
	}
}

// The same suite and run id in two instances must be two different databases, within the identifier limit.
func TestShardNamesOfTwoInstancesDiffer(t *testing.T) {
	if ShardName("inst-a", "web", "r1", 3) == ShardName("inst-b", "web", "r1", 3) {
		t.Error("two instances got the same shard database")
	}
	if SharedName("inst-a", "web", "r1") == SharedName("inst-b", "web", "r1") {
		t.Error("two instances got the same shared database")
	}
	if n := ShardName("inst-a", "a-suite-with-a-very-long-name-that-goes-on-and-on", "2026-11-19T10:00:00Z-and-more", 12); len(n) > 63 {
		t.Errorf("%q is %d bytes, over Postgres's limit", n, len(n))
	}
}

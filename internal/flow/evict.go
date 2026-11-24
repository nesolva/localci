package flow

import (
	"context"
	"fmt"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/suite"
	"gopkg.in/yaml.v3"
)

// DefaultMaxSize is the cap on one suite's checkpoints (files and database snapshots).
const DefaultMaxSize = 5 << 30

// maxSizeOf reads checkpoints.max_size from a suite's mapping, DefaultMaxSize when it is not set or not a size.
func maxSizeOf(n *yaml.Node) int64 {
	if n == nil || n.Kind != yaml.MappingNode {
		return DefaultMaxSize
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value != "checkpoints" || n.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		m := n.Content[i+1]
		for j := 0; j+1 < len(m.Content); j += 2 {
			if m.Content[j].Value == "max_size" {
				if v, err := config.ParseSize(m.Content[j+1].Value); err == nil && v > 0 {
					return int64(v)
				}
			}
		}
	}
	return DefaultMaxSize
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KiB", (n+1023)/1024)
}

// evict removes the least recently used checkpoints of the suite until the rest fits in limit, never the one just
// saved (keep) and never one in use: a save or a restore holds the key's lock, and the eviction (TryLock) skips it
// instead of waiting, so a snapshot is never dropped under a restore that is cloning it. Their databases go through
// the driver's guarded Drop, which refuses names that are not localci's.
func (s *Service) evict(ctx context.Context, hr suite.HookRun, limit int64, keep string) {
	all, err := s.Store.Checkpoints(ctx, hr.Suite) // least recently used first
	if err != nil {
		return
	}
	var total int64
	for _, c := range all {
		total += c.Bytes
	}
	for _, c := range all {
		if total <= limit {
			return
		}
		if c.Key == keep {
			continue
		}
		unlock, ok := s.tryLock(hr.Suite, c.Key)
		if !ok {
			continue // in use; the next save evicts it if it is still the least recently used
		}
		s.forget(ctx, hr, c)
		unlock()
		total -= c.Bytes
		hr.Emit("checkpoint.evicted", map[string]any{"flow": c.Flow, "key": c.Key[:16], "bytes": c.Bytes})
	}
}

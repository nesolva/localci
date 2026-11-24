package checks

import (
	"context"
	"sort"
	"sync"
)

// tagLocks serializes checks that build or run the same Docker tag: two
// `docker build -t app:scan .` side by side would overwrite each other's image
// while the other is still using it.
type tagLocks struct {
	mu   sync.Mutex
	held map[string]chan struct{} // closed when the holder lets go
}

// lock waits for every tag, always in the same order so two checks sharing two
// tags cannot wait for each other, and returns the function that frees them.
func (t *tagLocks) lock(ctx context.Context, tags []string) (func(), error) {
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	var got []string
	unlock := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for _, tag := range got {
			close(t.held[tag])
			delete(t.held, tag)
		}
	}
	for i, tag := range sorted {
		if i > 0 && tag == sorted[i-1] {
			continue
		}
		for {
			t.mu.Lock()
			if t.held == nil {
				t.held = map[string]chan struct{}{}
			}
			c, busy := t.held[tag]
			if !busy {
				t.held[tag] = make(chan struct{})
				got = append(got, tag)
				t.mu.Unlock()
				break
			}
			t.mu.Unlock()
			select {
			case <-c:
			case <-ctx.Done():
				unlock()
				return nil, context.Cause(ctx)
			}
		}
	}
	return unlock, nil
}

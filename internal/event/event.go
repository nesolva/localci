// Package event is the append-only event log every reader follows: the store holds the truth, the bus only wakes
// readers, so a subscriber replays from a cursor and then follows live with no gap and no duplicate, across restarts.
package event

import (
	"context"
	"sync"

	"github.com/nesolva/localci/internal/store"
)

// Event is one row of the log; Seq is assigned on Append.
type Event = store.Event

// Bus wakes subscribers after an append. It carries no events itself.
type Bus struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[chan struct{}]struct{}{}} }

func (b *Bus) watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Notify wakes every subscriber; wake-ups coalesce, so it never blocks.
func (b *Bus) Notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Log appends to a store and wakes the bus.
type Log struct {
	st  *store.Store
	bus *Bus
}

// NewLog ties a store to a bus; pass the same bus to SSE handlers.
func NewLog(st *store.Store, bus *Bus) *Log { return &Log{st: st, bus: bus} }

// Append persists the event, then wakes subscribers, and returns its seq.
func (l *Log) Append(ctx context.Context, e Event) (int64, error) {
	seq, err := l.st.AppendEvent(ctx, e)
	if err != nil {
		return 0, err
	}
	l.bus.Notify()
	return seq, nil
}

// Subscribe is Subscribe(ctx, store, bus, since) for this log.
func (l *Log) Subscribe(ctx context.Context, since int64) <-chan Event {
	return Subscribe(ctx, l.st, l.bus, since)
}

const batch = 500

// Subscribe delivers every event with seq > since exactly once and in order: first what the store already holds,
// then new appends as they happen. The channel closes when ctx ends or the store fails. A reader that is slow
// never blocks appenders; it just catches up from the store.
func Subscribe(ctx context.Context, st *store.Store, bus *Bus, since int64) <-chan Event {
	out := make(chan Event, 64)
	// Register before the first read: an append after this point leaves a wake-up pending, so nothing falls
	// between the read and the wait.
	wake, cancel := bus.watch()
	go func() {
		defer close(out)
		defer cancel()
		last := since
		for {
			evs, err := st.EventsAfter(ctx, last, batch)
			if err != nil {
				return
			}
			for _, e := range evs {
				select {
				case out <- e:
					last = e.Seq
				case <-ctx.Done():
					return
				}
			}
			if len(evs) == batch {
				continue
			}
			select {
			case <-wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

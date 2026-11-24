package event

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/store"
)

const heartbeat = 15 * time.Second

// SSE serves the event log as server-sent events. The cursor is the Last-Event-ID header when the client
// reconnects, else ?since=<seq>, else 0 (everything). Each message is `id: <seq>`, `event: <kind>` and the event as
// JSON in `data:`.
func SSE(st *store.Store, bus *Bus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since, err := cursor(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rc := http.NewResponseController(w)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "retry: 2000\n\n")
		if rc.Flush() != nil {
			return
		}

		ctx := r.Context()
		evs := Subscribe(ctx, st, bus, since)
		tick := time.NewTicker(heartbeat)
		defer tick.Stop()
		for {
			select {
			case e, ok := <-evs:
				if !ok {
					return
				}
				frame, ok := frameOf(e)
				if !ok {
					continue
				}
				if _, err := w.Write(frame); err != nil {
					return
				}
				// Flush only when the burst is over, so a replay of thousands is not one syscall each.
				if len(evs) == 0 && rc.Flush() != nil {
					return
				}
			case <-tick.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
}

// frameOf is the SSE message of one event. The kind sits on its own line, so line breaks are stripped from it
// (the store already refuses them; rows written by older versions may still hold some).
func frameOf(e Event) ([]byte, bool) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, false
	}
	kind := strings.NewReplacer("\r", "", "\n", "").Replace(e.Kind)
	return []byte(fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", e.Seq, kind, data)), true
}

func cursor(r *http.Request) (int64, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("since")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("since must be a non-negative event number, got %q", v)
	}
	return n, nil
}

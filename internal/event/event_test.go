package event

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
)

func newLog(t *testing.T) (*store.Store, *Bus, *Log) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := NewBus()
	return st, bus, NewLog(st, bus)
}

// A subscriber that joins mid-stream, while many writers append, gets every later event once, in order.
func TestReplayWhileAppending(t *testing.T) {
	st, bus, log := newLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const writers, per, head = 8, 1250, 1500
	for i := 0; i < head; i++ {
		if _, err := log.Append(ctx, Event{Kind: "pre", Run: "r1"}); err != nil {
			t.Fatal(err)
		}
	}
	since := int64(head / 2)
	sub := Subscribe(ctx, st, bus, since)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				data := json.RawMessage(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i))
				if _, err := log.Append(ctx, Event{Kind: "load", Run: "r1", Shard: w + 1, Data: data}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	total := int64(head + writers*per)
	want := since + 1
	for want <= total {
		select {
		case e, ok := <-sub:
			if !ok {
				t.Fatalf("subscription closed at seq %d, want %d", want, total)
			}
			if e.Seq != want {
				t.Fatalf("got seq %d, want %d (gap or duplicate)", e.Seq, want)
			}
			want++
		case <-ctx.Done():
			t.Fatalf("timed out at seq %d of %d", want, total)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("append failed (busy database?): %v", err)
	}
	select {
	case e := <-sub:
		t.Fatalf("extra event after the last: %+v", e)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSubscribeStopsWithContext(t *testing.T) {
	st, bus, _ := newLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	sub := Subscribe(ctx, st, bus, 0)
	cancel()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("event from an empty log")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

type sseClient struct {
	resp *http.Response
	sc   *bufio.Scanner
}

func connect(t *testing.T, url, lastID string) *sseClient {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return &sseClient{resp: resp, sc: bufio.NewScanner(resp.Body)}
}

// next returns the next event's id and kind, failing if none arrives within d.
func (c *sseClient) next(t *testing.T, d time.Duration) (int64, string) {
	t.Helper()
	type msg struct {
		id   int64
		kind string
	}
	got := make(chan msg, 1)
	go func() {
		var m msg
		for c.sc.Scan() {
			line := c.sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				m.id, _ = strconv.ParseInt(line[4:], 10, 64)
			case strings.HasPrefix(line, "event: "):
				m.kind = line[7:]
			case line == "" && m.id != 0:
				got <- m
				return
			}
		}
	}()
	select {
	case m := <-got:
		return m.id, m.kind
	case <-time.After(d):
		t.Fatalf("no SSE event within %v", d)
		return 0, ""
	}
}

func TestSSELiveAndResume(t *testing.T) {
	st, bus, log := newLog(t)
	srv := httptest.NewServer(SSE(st, bus))
	t.Cleanup(srv.Close) // runs after the clients are closed (cleanups are LIFO)
	ctx := context.Background()

	a := connect(t, srv.URL, "")
	if ct := a.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	seq, _ := log.Append(ctx, Event{Kind: "run.queued", Run: "r1"})
	if id, kind := a.next(t, time.Second); id != seq || kind != "run.queued" {
		t.Fatalf("got %d %s, want %d run.queued", id, kind, seq)
	}
	a.resp.Body.Close()

	// Events written while the client is away arrive after reconnecting with Last-Event-ID, nothing repeated.
	var last int64
	for i := 0; i < 3; i++ {
		last, _ = log.Append(ctx, Event{Kind: "test.end", Run: "r1"})
	}
	b := connect(t, srv.URL, strconv.FormatInt(seq, 10))
	for want := seq + 1; want <= last; want++ {
		if id, _ := b.next(t, time.Second); id != want {
			t.Fatalf("resumed at %d, want %d", id, want)
		}
	}
	next, _ := log.Append(ctx, Event{Kind: "run.passed", Run: "r1"})
	if id, kind := b.next(t, time.Second); id != next || kind != "run.passed" {
		t.Fatalf("live after resume: got %d %s", id, kind)
	}

	// ?since works for the first connection of a client with no Last-Event-ID.
	c := connect(t, srv.URL+"?since="+strconv.FormatInt(last, 10), "")
	if id, _ := c.next(t, time.Second); id != next {
		t.Fatalf("?since: got %d, want %d", id, next)
	}
}

func TestSSERejectsBadCursor(t *testing.T) {
	st, bus, _ := newLog(t)
	srv := httptest.NewServer(SSE(st, bus))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "?since=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestFrameHasExactlyOneIDLine(t *testing.T) {
	// Rows written before kinds were validated may hold line breaks; they must not forge an `id:` line.
	frame, ok := frameOf(Event{Seq: 7, Kind: "x\r\nid: 999999999", Run: "r"})
	if !ok {
		t.Fatal("no frame")
	}
	ids := 0
	for _, l := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(l, "id:") {
			ids++
		}
	}
	if ids != 1 || !strings.HasPrefix(string(frame), "id: 7\n") {
		t.Fatalf("frame = %q", frame)
	}
}

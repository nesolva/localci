package report

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

type collector struct {
	mu  sync.Mutex
	evs []Event
	bad int
}

func (c *collector) event(e Event)         { c.mu.Lock(); c.evs = append(c.evs, e); c.mu.Unlock() }
func (c *collector) badLine([]byte, error) { c.mu.Lock(); c.bad++; c.mu.Unlock() }
func (c *collector) count() int            { c.mu.Lock(); defer c.mu.Unlock(); return len(c.evs) }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestTailFollowsAFileCreatedLateAndHoldsPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.ndjson")
	c := &collector{}
	tl := &Tailer{Path: path, Interval: 5 * time.Millisecond, OnEvent: c.event, OnBad: c.badLine}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()

	time.Sleep(30 * time.Millisecond) // the tailer is already polling a file that does not exist
	appendTo(t, path, `{"v":1,"t":"begin","ts":1,"shard":2,"total":3}`+"\n"+`{"v":1,"t":"test.be`)
	waitFor(t, "first event", func() bool { return c.count() == 1 })
	time.Sleep(30 * time.Millisecond)
	if c.count() != 1 {
		t.Fatalf("a half-written line was delivered: %d events", c.count())
	}
	appendTo(t, path, `gin","ts":2,"test":"a","file":"f.spec.ts","title":"t","line":4}`+"\n")
	waitFor(t, "completed line", func() bool { return c.count() == 2 })

	// cancelled right after the last write: the final read still delivers it
	appendTo(t, path, `{"v":1,"t":"end","ts":3,"status":"passed"}`+"\n")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if c.count() != 3 {
		t.Fatalf("got %d events, want 3", c.count())
	}
	if b := c.evs[0].Body.(*Begin); b.Shard != 2 || b.Total != 3 {
		t.Errorf("begin = %+v", b)
	}
	if tb := c.evs[1].Body.(*TestBegin); tb.Test != "a" || tb.Line != 4 {
		t.Errorf("test.begin = %+v", tb)
	}
}

func TestUnknownTypesAreKeptAndGarbageIsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.ndjson")
	appendTo(t, path,
		`{"v":1,"t":"future.thing","ts":5,"x":[1,2]}`+"\n"+
			"not json at all\n"+
			`{"v":2,"t":"begin","ts":6,"shard":1,"total":1}`+"\n"+
			`{"v":1,"t":"end","ts":7,"status":"failed"}`)
	c := &collector{}
	tl := &Tailer{Path: path, OnEvent: c.event, OnBad: c.badLine}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the file is complete already: one final read must get everything, even an unterminated last line
	if err := tl.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(c.evs) != 3 || c.bad != 1 {
		t.Fatalf("events=%d bad=%d, want 3 and 1", len(c.evs), c.bad)
	}
	if u := c.evs[0]; u.Known() || u.Type != "future.thing" || string(u.Raw) != `{"v":1,"t":"future.thing","ts":5,"x":[1,2]}` {
		t.Errorf("unknown event not kept as written: %+v", u)
	}
	if c.evs[1].Known() {
		t.Error("an event of another protocol version must not be decoded as v1")
	}
	if e := c.evs[2].Body.(*End); e.Status != "failed" {
		t.Errorf("end = %+v", e)
	}
}

func TestTailRestartsWhenTheFileIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.ndjson")
	appendTo(t, path, `{"v":1,"t":"begin","ts":1,"shard":1,"total":1}`+"\n"+`{"v":1,"t":"begin","ts":2,"shard":1,"total":1}`+"\n")
	c := &collector{}
	tl := &Tailer{Path: path, Interval: 5 * time.Millisecond, OnEvent: c.event}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()
	waitFor(t, "two events", func() bool { return c.count() == 2 })
	if err := os.WriteFile(path, []byte(`{"v":1,"t":"end","ts":3,"status":"passed"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "event after truncation", func() bool { return c.count() == 3 })
	cancel()
	<-done
}

// testdata/sample.ndjson is a real run of examples/acme-shop with ACME_BREAK=1
// (packages/playwright: npm run sample:record).
func TestGoldenSampleRunParsesIntoTypedEvents(t *testing.T) {
	evs, err := ReadFile("testdata/sample.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, e := range evs {
		if !e.Known() {
			t.Fatalf("event %q has no typed body", e.Type)
		}
		counts[e.Type]++
	}
	if len(evs) < 100 || counts["begin"] != 1 || counts["end"] != 1 || counts["test.begin"] != 13 || counts["test.end"] != 13 {
		t.Fatalf("unexpected shape: %d events, %v", len(evs), counts)
	}
	if counts["step.begin"] != counts["step.end"] {
		t.Errorf("steps: %d begin, %d end", counts["step.begin"], counts["step.end"])
	}
	if b := evs[0].Body.(*Begin); b.Total != 13 {
		t.Errorf("begin = %+v", b)
	}

	var failed []*TestEnd
	for _, e := range evs {
		if te, ok := e.Body.(*TestEnd); ok && te.Status == "failed" {
			failed = append(failed, te)
		}
	}
	if len(failed) != 1 || failed[0].Failure == nil {
		t.Fatalf("want exactly one failed test with a failure, got %+v", failed)
	}
	f := failed[0].Failure
	if f.Expected == nil || *f.Expected != `"Total: $27.50"` || f.Actual == nil || *f.Actual != `"Total: $17.50"` {
		t.Errorf("expected/actual = %v / %v", f.Expected, f.Actual)
	}
	if f.Location == nil || f.Aria == nil || f.Screenshot == nil || f.Trace == nil || len(f.Console) == 0 || len(f.Network) == 0 {
		t.Errorf("failure is missing parts: %+v", f)
	}
	if f.Network[0].Status != 422 || f.Network[0].Method != "POST" {
		t.Errorf("network = %+v", f.Network)
	}
}

func TestSanitizeStripsSecretsAndBoundsText(t *testing.T) {
	long := strings.Repeat("é", 3000)
	f := Failure{
		Message: long,
		Console: []string{long},
		Network: []NetworkError{
			{Method: "GET", URL: "https://u:pw@api.test/cb?code=SECRET&t=1#frag", Status: 401},
			{Method: "GET", URL: "/rel?token=SECRET", Status: 500},
		},
	}
	f.Sanitize()
	if f.Network[0].URL != "https://api.test/cb" || f.Network[1].URL != "/rel" {
		t.Errorf("urls = %q, %q", f.Network[0].URL, f.Network[1].URL)
	}
	if len(f.Console[0]) > 510 || len(f.Message) > 2010 || !utf8.ValidString(f.Console[0]) {
		t.Errorf("not bounded: console %d, message %d", len(f.Console[0]), len(f.Message))
	}
}

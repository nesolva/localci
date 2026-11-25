package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

// fakeBin installs an executable called name that records its arguments, one per line, and returns the file they
// go to. PATH holds only that directory, so the notifier cannot find a real tool.
func fakeBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	for _, name := range names {
		script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + out + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return out
}

func sentArgs(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the notifier did not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestSendRunsThePlatformsTool(t *testing.T) {
	m := Message{Title: `e2e "failed"`, Body: "#42 · 2 of 128 tests failed", URL: "http://127.0.0.1:7357/runs/42"}
	t.Run("macOS with terminal-notifier opens the run on click", func(t *testing.T) {
		f := fakeBin(t, "terminal-notifier", "osascript")
		n := New()
		n.goos = "darwin"
		if err := n.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		want := []string{"-title", m.Title, "-message", m.Body, "-group", "localci", "-open", m.URL}
		if got := sentArgs(t, f); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("args %q, want %q", got, want)
		}
	})
	t.Run("macOS without it falls back to osascript with the text as arguments", func(t *testing.T) {
		f := fakeBin(t, "osascript")
		n := New()
		n.goos = "darwin"
		if err := n.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		got := sentArgs(t, f)
		if len(got) < 4 || got[0] != "-e" || got[len(got)-2] != m.Title || got[len(got)-1] != m.Body {
			t.Fatalf("args %q", got)
		}
		if strings.Contains(got[1], "failed") || strings.Contains(got[1], m.Body) {
			t.Errorf("the text was spliced into the script: %q", got[1])
		}
	})
	t.Run("Linux uses notify-send", func(t *testing.T) {
		f := fakeBin(t, "notify-send")
		n := New()
		n.goos = "linux"
		if err := n.Send(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		want := []string{"--app-name=localci", "--", m.Title, m.Body}
		if got := sentArgs(t, f); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("args %q, want %q", got, want)
		}
	})
	t.Run("no tool is an error, not a silent drop", func(t *testing.T) {
		fakeBin(t)
		n := New()
		n.goos = "linux"
		if err := n.Send(context.Background(), m); !errors.Is(err, ErrNoNotifier) {
			t.Errorf("err = %v", err)
		}
	})
}

type recorder struct{ got chan Message }

func (r recorder) Send(_ context.Context, m Message) error { r.got <- m; return nil }

// Watch tells the person who started a run how it ended, only for the outcomes they asked for.
func TestWatchNotifiesFinishedRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "localci.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus := event.NewBus()
	log := event.NewLog(st, bus)
	rec := recorder{got: make(chan Message, 8)}
	go Watch(ctx, st, bus, []string{"failed"}, rec, func(id string) string { return "http://dash/runs/" + id }, t.Logf)
	time.Sleep(50 * time.Millisecond) // Watch starts following from the log's end

	finish := func(id, trigger, state, infra string) {
		t.Helper()
		now := time.Now().UnixMilli()
		if err := st.PutRun(ctx, store.Run{ID: id, Suite: "e2e", Kind: store.KindSuite, Trigger: trigger, State: state,
			Infra: infra, Created: now - 90_000, Started: now - 80_000, Finished: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := log.Append(ctx, store.Event{Run: id, Kind: "run.state", Data: []byte(`{"state":"` + state + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	finish("1", "manual", "passed", "")    // not asked for
	finish("2", "push", "failed", "")      // started by a push, not by hand
	finish("3", "manual", "cancelled", "") // never notifies
	finish("4", "retry", "failed", "")

	select {
	case m := <-rec.got:
		if m.Title != "e2e failed" || !strings.HasPrefix(m.Body, "#4") || m.URL != "http://dash/runs/4" {
			t.Errorf("message %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed retry did not notify")
	}
	select {
	case m := <-rec.got:
		t.Errorf("extra notification %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestDescribeInfrastructureFailure(t *testing.T) {
	m := Describe(store.Run{ID: "9", Suite: "e2e", State: store.StateFailed, Infra: "stack exited", Started: 1, Finished: 60_001}, 0, 0)
	if m.Title != "e2e could not run" || m.Body != "#9 · stack exited · 1m00s" {
		t.Errorf("%+v", m)
	}
}

// A run that failed on timeouts or interruptions failed those tests: the banner counts them as the dashboard and
// the pull request status do.
func TestWatchCountsTimedOutAndInterruptedTestsAsFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(filepath.Join(t.TempDir(), "localci.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus := event.NewBus()
	log := event.NewLog(st, bus)
	rec := recorder{got: make(chan Message, 8)}
	go Watch(ctx, st, bus, []string{"failed"}, rec, nil, t.Logf)
	time.Sleep(50 * time.Millisecond) // Watch starts following from the log's end

	now := time.Now().UnixMilli()
	if err := st.PutRun(ctx, store.Run{ID: "5", Suite: "e2e", Kind: store.KindSuite, Trigger: "manual",
		State: store.StateFailed, Created: now - 90_000, Started: now - 80_000, Finished: now}); err != nil {
		t.Fatal(err)
	}
	for id, status := range map[string]string{"a": "passed", "b": "timedOut", "c": "interrupted"} {
		if err := st.PutTest(ctx, store.Test{Run: "5", Shard: 1, ID: id, Title: id, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := log.Append(ctx, store.Event{Run: "5", Kind: "run.state", Data: []byte(`{"state":"failed"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-rec.got:
		if !strings.Contains(m.Body, "2 of 3 tests failed") {
			t.Errorf("banner %q, want it to say 2 of 3 tests failed", m.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failed run did not notify")
	}
}

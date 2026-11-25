package server

import (
	"context"
	"encoding/json"
	"errors"
	osexec "os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

type posted struct {
	repo, sha string
	st        forge.Status
}

// recordForge keeps the statuses it is asked to post.
type recordForge struct {
	mu  sync.Mutex
	got []posted
}

func (f *recordForge) Watch(context.Context, func(forge.Trigger) error) error { return nil }
func (f *recordForge) ResolveHeads(context.Context, []string, forge.Trigger) ([]forge.Head, error) {
	return nil, nil
}
func (f *recordForge) SetStatus(_ context.Context, repo, sha string, st forge.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, posted{repo, sha, st})
	return nil
}
func (f *recordForge) statuses() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.got...)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestReportSeesARunThatEndedBeforeItStartedFollowing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := openStore(t)
	bus := event.NewBus()
	elog := event.NewLog(st, bus)
	fg := &recordForge{}
	d := &Dispatcher{Forge: fg, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id }}

	since, _ := st.LastSeq(ctx) // taken before the run is submitted
	if err := st.PutRun(ctx, store.Run{ID: "9", Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"state": store.StatePassed})
	elog.Append(ctx, store.Event{Run: "9", Kind: "run.state", Data: data}) // the whole run is over before report starts

	done := make(chan struct{})
	go func() {
		d.report(ctx, "9", "e2e", []forge.Head{{Repo: "acme/web", SHA: "abc", PR: 1}}, since)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("report never saw the run finish: the pull request would stay queued")
	}
	got := fg.statuses()
	if len(got) < 2 || got[len(got)-1].st.State != forge.StateSuccess {
		t.Fatalf("statuses = %+v; want queued, then success", got)
	}
}

// GitHub's pull request refs still show an older commit than the pushed head:
// Handle must hand the trigger back to the watcher (ErrNotReady) without
// posting anything, and consume it on the watcher's last attempt.
func TestHandleLeavesAHeadGitHubHasNotCaughtUpWithToTheNextPoll(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	src, data := t.TempDir(), t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := osexec.Command("git", append([]string{"-C", src, "-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "old head")
	git("update-ref", "refs/pull/1/head", git("rev-parse", "HEAD"))

	st := openStore(t)
	fg := &recordForge{}
	cr := &checks.Runner{Store: st, Forge: fg, Options: checks.Options{DataDir: data},
		Mirror: func(ctx context.Context, repo string) (*exec.Mirror, error) {
			return exec.EnsureMirror(ctx, data, repo, src)
		}}
	d := &Dispatcher{Forge: fg, Checks: cr, Store: st,
		Config: func() *config.Config { return &config.Config{Repos: []config.Repo{{Name: "acme/api", Checks: true}}} }}
	tr := forge.Trigger{Kind: forge.KindPush, Repo: "acme/api", PR: 1, Base: "main",
		Head: strings.Repeat("1", 40), Attempt: 1}
	if err := d.Handle(t.Context(), tr); !errors.Is(err, forge.ErrNotReady) {
		t.Fatalf("Handle = %v, want ErrNotReady so the next poll retries", err)
	}
	if got := fg.statuses(); len(got) != 0 {
		t.Fatalf("statuses for a head that was not tested: %+v", got)
	}
	tr.Attempt, tr.LastAttempt = forge.MaxAttempts, true
	if err := d.Handle(t.Context(), tr); err != nil {
		t.Fatalf("the last attempt must be consumed, got %v", err)
	}
}

// A run whose stack or tenant flow broke posts an error that says so, never a failure: its interrupted tests are
// not counted as failed, so the pull request is not told that N tests are wrong.
func TestInfraRunPostsErrorNotFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := openStore(t)
	bus := event.NewBus()
	elog := event.NewLog(st, bus)
	fg := &recordForge{}
	d := &Dispatcher{Forge: fg, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id }}

	since, _ := st.LastSeq(ctx)
	st.PutRun(ctx, store.Run{ID: "9", Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed,
		Reason: "infrastructure: tenant flow make failed: boom", Infra: "tenant flow make failed: boom"})
	st.PutTest(ctx, store.Test{Run: "9", ID: "a", Title: "a", Status: "passed"})
	st.PutTest(ctx, store.Test{Run: "9", ID: "b", Title: "b", Status: "interrupted"})
	data, _ := json.Marshal(map[string]string{"state": store.StateFailed})
	elog.Append(ctx, store.Event{Run: "9", Kind: "run.state", Data: data})

	d.report(ctx, "9", "e2e", []forge.Head{{Repo: "acme/web", SHA: "abc", PR: 1}}, since)
	got := fg.statuses()
	last := got[len(got)-1].st
	if last.State != forge.StateError || !strings.HasPrefix(last.Description, "infrastructure:") {
		t.Fatalf("last status = %+v, want error starting with \"infrastructure:\"", last)
	}
}

// subscribers counts the goroutines following the event log.
func subscribers() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "event.Subscribe.func")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// A report ends with its run; what it followed the run with must end too, or every pull request run leaves a
// goroutine and a bus slot behind for the life of serve.
func TestFinishedReportsLeaveNoSubscriptionBehind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := openStore(t)
	bus := event.NewBus()
	elog := event.NewLog(st, bus)
	d := &Dispatcher{Forge: &recordForge{}, Store: st, Bus: bus, URL: func(id string) string { return "http://x/runs/" + id }}

	before := subscribers()
	for i := range 20 {
		id := strconv.Itoa(i + 1)
		since, _ := st.LastSeq(ctx)
		st.PutRun(ctx, store.Run{ID: id, Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed})
		data, _ := json.Marshal(map[string]string{"state": store.StatePassed})
		elog.Append(ctx, store.Event{Run: id, Kind: "run.state", Data: data})
		d.report(ctx, id, "e2e", []forge.Head{{Repo: "acme/web", SHA: "abc", PR: 1}}, since)
	}
	deadline := time.Now().Add(2 * time.Second)
	for subscribers() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if left := subscribers() - before; left > 0 {
		t.Fatalf("%d of 20 finished reports still follow the event log", left)
	}
}

package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
)

// The token goes to the API host only, whatever a Link header points at.
func TestNextPageOnAnotherHostIsNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization") != "")
		writeJSON(w, []any{})
	}))
	defer other.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/repos/acme/api/pulls/1/reviews?page=2>; rel="next"`)
		writeJSON(w, []any{})
	}))
	defer api.Close()
	g := NewGitHub(Options{Token: "secret", BaseURL: api.URL, Clock: &fakeClock{now: t0}})
	err := g.getAll(context.Background(), "/repos/acme/api/pulls/1/reviews", 5, func(json.RawMessage) error { return nil })
	if leaked.Load() || err == nil {
		t.Fatalf("followed a next page on another host (err %v, token sent %v)", err, leaked.Load())
	}
}

func pr(n int, sha string, labels ...string) fakePR {
	return fakePR{Number: n, Updated: t0, Branch: "feat-" + sha, SHA: sha, Base: "main", Author: "dev", Labels: labels}
}

func kinds(ts []Trigger) []string {
	var out []string
	for _, t := range ts {
		out = append(out, string(t.Kind)+":"+t.Head)
	}
	return out
}

func eq(t *testing.T, got []Trigger, want ...string) {
	t.Helper()
	g := kinds(got)
	if len(g) != len(want) {
		t.Fatalf("triggers = %v, want %v", g, want)
	}
	for i := range g {
		if g[i] != want[i] {
			t.Fatalf("triggers = %v, want %v", g, want)
		}
	}
}

func TestWatchPollsWithETagAndEmitsOncePerHeadAndLabel(t *testing.T) {
	f := newFake(t)
	f.set("acme/api", pr(1, "aaa"))
	g := f.client(Options{Repos: []string{"acme/api"}}, &fakeClock{now: t0})

	var got []Trigger
	emit := func(tr Trigger) error { got = append(got, tr); return nil }

	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:aaa")
	if got[0].Repo != "acme/api" || got[0].PR != 1 || got[0].Branch != "feat-aaa" || got[0].Base != "main" || got[0].Author != "dev" {
		t.Fatalf("trigger fields wrong: %+v", got[0])
	}

	got = nil
	g.pollOnce(context.Background(), emit) // nothing changed: 304
	eq(t, got)
	if n := f.count("GET /repos/acme/api/pulls"); n != 2 {
		t.Fatalf("expected 2 list calls, got %d", n)
	}

	got = nil
	f.set("acme/api", pr(1, "bbb")) // new head
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:bbb")

	got = nil
	f.set("acme/api", pr(1, "bbb", "e2e")) // label added, same head
	g.pollOnce(context.Background(), emit)
	eq(t, got, "label:bbb")
	if got[0].Label != "e2e" {
		t.Fatalf("label trigger carries %q", got[0].Label)
	}

	got = nil
	f.set("acme/api", pr(1, "bbb", "e2e", "other")) // unrelated change
	g.pollOnce(context.Background(), emit)
	eq(t, got)

	got = nil
	f.set("acme/api", pr(1, "bbb")) // label removed ...
	g.pollOnce(context.Background(), emit)
	f.set("acme/api", pr(1, "bbb", "e2e")) // ... and added again
	g.pollOnce(context.Background(), emit)
	eq(t, got, "label:bbb")
}

// A suite's `trigger.label` may name any label: the watcher built from the configuration reports each suite's label
// when it is added, spelled as the configuration spells it (the dispatcher compares it as is).
func TestWatchReportsEverySuitesConfiguredLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := `version: 1
repos:
  - name: acme/api
    checks: false
suites:
  full:
    repos: [acme/api]
    trigger: {label: run-e2e}
  nightly:
    repos: [acme/api]
    trigger: {push: true, label: Nightly}
  smoke:
    repos: [acme/api]
    trigger: {push: true}
    test: {repo: acme/api, run: make smoke}
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	eff, err := config.Load(config.Options{Path: path, Env: func(string) string { return "" }, Machine: capacity.Machine{CPUs: 4}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	f.set("acme/api", pr(1, "aaa"))
	g := f.client(OptionsFromConfig(&eff.Config, "token"), &fakeClock{now: t0})
	var got []Trigger
	emit := func(tr Trigger) error { got = append(got, tr); return nil }
	labels := func() []string {
		var out []string
		for _, tr := range got {
			if tr.Kind == KindLabel {
				out = append(out, tr.Label)
			}
		}
		return out
	}

	g.pollOnce(context.Background(), emit)
	got = nil
	f.set("acme/api", pr(1, "aaa", "nightly", "e2e")) // GitHub matches labels without case
	g.pollOnce(context.Background(), emit)
	if l := labels(); len(l) != 1 || l[0] != "Nightly" {
		t.Fatalf("label triggers %v, want [Nightly]", l)
	}

	got = nil
	f.set("acme/api", pr(1, "aaa", "nightly", "e2e", "run-e2e")) // a second suite's label, the first one still on
	g.pollOnce(context.Background(), emit)
	if l := labels(); len(l) != 1 || l[0] != "run-e2e" {
		t.Fatalf("label triggers %v, want [run-e2e]", l)
	}

	got = nil
	f.set("acme/api", pr(1, "aaa", "run-e2e")) // Nightly removed ...
	g.pollOnce(context.Background(), emit)
	f.set("acme/api", pr(1, "aaa", "run-e2e", "Nightly")) // ... and added again
	g.pollOnce(context.Background(), emit)
	if l := labels(); len(l) != 1 || l[0] != "Nightly" {
		t.Fatalf("label triggers %v, want [Nightly] once", l)
	}
}

// GitHub's pull request refs can lag behind the head the list shows: a handler
// that could not test the head yet must see it again on the next poll, even
// when the list itself did not change, and not for ever.
func TestWatchReemitsAHeadItsHandlerWasNotReadyFor(t *testing.T) {
	f := newFake(t)
	f.set("acme/api", pr(1, "aaa"))
	g := f.client(Options{Repos: []string{"acme/api"}}, &fakeClock{now: t0})

	var got []Trigger
	ready := false
	emit := func(tr Trigger) error {
		got = append(got, tr)
		if tr.Kind == KindPush && !ready {
			return fmt.Errorf("checks: %w", ErrNotReady)
		}
		return nil
	}
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:aaa")
	got = nil
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:aaa")
	if got[0].Attempt != 2 || got[0].LastAttempt {
		t.Fatalf("second emission = %+v, want attempt 2 of %d", got[0], MaxAttempts)
	}
	ready = true
	got = nil
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:aaa")
	got = nil
	g.pollOnce(context.Background(), emit)
	eq(t, got) // handled: consumed

	// A head that never gets ready is given up after MaxAttempts polls.
	ready = false
	f.set("acme/api", pr(1, "bbb"))
	got = nil
	for i := 0; i < MaxAttempts+2; i++ {
		g.pollOnce(context.Background(), emit)
	}
	if len(got) != MaxAttempts || !got[MaxAttempts-1].LastAttempt || got[MaxAttempts-2].LastAttempt {
		t.Fatalf("%d emissions (last %+v), want %d with only the last one marked", len(got), got[len(got)-1], MaxAttempts)
	}
}

func TestWatchSkipsDraftsAndOldPRsUntilTheyQualify(t *testing.T) {
	f := newFake(t)
	draft, old := pr(1, "ddd"), pr(2, "ooo")
	draft.Draft = true
	old.Updated = t0.Add(-25 * time.Hour)
	f.set("acme/api", draft, old)
	g := f.client(Options{Repos: []string{"acme/api"}, MaxAge: 24 * time.Hour}, &fakeClock{now: t0})

	var got []Trigger
	emit := func(tr Trigger) error { got = append(got, tr); return nil }
	g.pollOnce(context.Background(), emit)
	eq(t, got)

	ready := pr(1, "ddd") // marked ready for review
	f.set("acme/api", ready, old)
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:ddd")
}

func TestWatchFailedForkCheckIsRetriedNotCachedAsNotModified(t *testing.T) {
	f := newFake(t)
	fork := pr(1, "fff", "ok-to-test")
	fork.Fork = true
	f.set("acme/api", fork)
	f.reviews["acme/api#1"] = []fakeReview{{User: "maint", State: "APPROVED", Commit: "fff"}}
	// no permission entry for "maint" yet and the endpoint 404s: denied, not an error
	g := f.client(Options{Repos: []string{"acme/api"}, Forks: "label"}, &fakeClock{now: t0})

	var got []Trigger
	emit := func(tr Trigger) error { got = append(got, tr); return nil }
	g.pollOnce(context.Background(), emit)
	eq(t, got)
	f.mu.Lock()
	f.perms["maint"] = "write"
	f.etag["acme/api"]++ // any change to the PR list
	f.mu.Unlock()
	g.pollOnce(context.Background(), emit)
	eq(t, got, "push:fff")
}

func TestWatchForkPolicy(t *testing.T) {
	const ok = "ok-to-test"
	approved := func(user, sha string) []fakeReview {
		return []fakeReview{{User: user, State: "APPROVED", Commit: sha}}
	}
	cases := []struct {
		name    string
		forks   string
		labels  []string
		reviews []fakeReview
		want    int // triggers on first poll
	}{
		{"never ignores even approved forks", "never", []string{ok}, approved("maint", "fff"), 0},
		{"label policy without the label", "label", nil, approved("maint", "fff"), 0},
		{"label without an approval", "label", []string{ok}, nil, 0},
		{"label and a writer's approval of the head", "label", []string{ok}, approved("maint", "fff"), 1},
		{"label and an admin's approval of the head", "label", []string{ok}, approved("boss", "fff"), 1},
		{"approval by a non-writer", "label", []string{ok}, approved("rando", "fff"), 0},
		{"approval of an older commit", "label", []string{ok}, approved("maint", "eee"), 0},
		{"approval later dismissed", "label", []string{ok},
			append(approved("maint", "fff"), fakeReview{User: "maint", State: "DISMISSED", Commit: "fff"}), 0},
		{"changes requested after approving", "label", []string{ok},
			append(approved("maint", "fff"), fakeReview{User: "maint", State: "CHANGES_REQUESTED", Commit: "fff"}), 0},
		{"a comment does not undo an approval", "label", []string{ok},
			append(approved("maint", "fff"), fakeReview{User: "maint", State: "COMMENTED", Commit: "fff"}), 1},
		{"always runs unlabelled forks", "always", nil, nil, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			fork := pr(1, "fff", c.labels...)
			fork.Fork = true
			f.set("acme/api", fork)
			f.perms["maint"], f.perms["rando"], f.perms["boss"] = "write", "read", "admin"
			f.reviews["acme/api#1"] = c.reviews
			g := f.client(Options{Repos: []string{"acme/api"}, Forks: c.forks}, &fakeClock{now: t0})
			if got := collect(g, 1, nil); len(got) != c.want {
				t.Fatalf("got %v, want %d trigger(s)", kinds(got), c.want)
			}
		})
	}
}

// GitHub forbids approving your own pull request, so a writer's PR from their
// own fork runs without a review. Whoever controls the head branch can push to
// it, so a writer's PR whose head is in someone else's fork still needs a
// writer's approval of its head.
func TestWatchForkByAWriterNeedsNoReviewOnlyFromTheirOwnFork(t *testing.T) {
	cases := []struct {
		name, author, headOwner string
		reviews                 []fakeReview
		want                    int
	}{
		{"writer from own fork", "maint", "maint", nil, 1},
		{"writer from own fork, login case differs", "maint", "Maint", nil, 1},
		{"non-writer from own fork", "rando", "rando", nil, 0},
		{"writer, head in another user's fork", "maint", "rando", nil, 0},
		{"writer, head in another user's fork, approved by a writer", "maint", "rando",
			[]fakeReview{{User: "boss", State: "APPROVED", Commit: "fff"}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			fork := pr(1, "fff", "ok-to-test")
			fork.Fork, fork.Author, fork.HeadOwner = true, c.author, c.headOwner
			f.set("acme/api", fork)
			f.perms["maint"], f.perms["rando"], f.perms["boss"] = "write", "read", "admin"
			f.reviews["acme/api#1"] = c.reviews
			g := f.client(Options{Repos: []string{"acme/api"}, Forks: "label"}, &fakeClock{now: t0})
			if got := collect(g, 1, nil); len(got) != c.want {
				t.Fatalf("got %v, want %d trigger(s)", kinds(got), c.want)
			}
		})
	}
}

// The approval is a writer's review on the exact head, which GitHub records:
// it holds across a close and reopen and a restart of localci, and a push made
// after it needs a new approval.
func TestWatchForkApprovalIsBoundToTheApprovedCommit(t *testing.T) {
	f := newFake(t)
	fork := func(sha string, labels ...string) fakePR {
		p := pr(1, sha, labels...)
		p.Fork = true
		return p
	}
	f.set("acme/api", fork("v1", "ok-to-test"))
	f.perms["maint"] = "write"
	f.reviews["acme/api#1"] = []fakeReview{{User: "maint", State: "APPROVED", Commit: "v1"}}
	opts := Options{Repos: []string{"acme/api"}, Forks: "label"}
	g := f.client(opts, &fakeClock{now: t0})
	eq(t, collect(g, 1, nil), "push:v1")

	f.set("acme/api", fork("v2", "ok-to-test")) // the fork author pushes after the approval
	eq(t, collect(g, 1, nil))
	f.set("acme/api") // ... closes the PR ...
	eq(t, collect(g, 1, nil))
	f.set("acme/api", fork("v2", "ok-to-test")) // ... and reopens it
	eq(t, collect(g, 1, nil))
	eq(t, collect(f.client(opts, &fakeClock{now: t0}), 1, nil)) // localci restarted

	f.mu.Lock() // a writer reviews v2 and approves it
	f.reviews["acme/api#1"] = append(f.reviews["acme/api#1"], fakeReview{User: "maint", State: "APPROVED", Commit: "v2"})
	f.etag["acme/api"]++
	f.mu.Unlock()
	eq(t, collect(g, 1, nil), "push:v2")

	f.set("acme/api", fork("v2", "e2e")) // ok-to-test removed: revoked, the e2e label runs nothing
	eq(t, collect(g, 1, nil))
}

func TestWatchForkPushedBeforeTheFirstPollIsNotApproved(t *testing.T) {
	f := newFake(t)
	p := pr(1, "v2", "ok-to-test") // approved at v1, v2 pushed before localci looked
	p.Fork = true
	f.set("acme/api", p)
	f.perms["maint"] = "write"
	f.reviews["acme/api#1"] = []fakeReview{{User: "maint", State: "APPROVED", Commit: "v1"}}
	g := f.client(Options{Repos: []string{"acme/api"}, Forks: "label"}, &fakeClock{now: t0})
	eq(t, collect(g, 1, nil))
}

func TestWatchRateLimitPausesUntilReset(t *testing.T) {
	f := newFake(t)
	f.set("acme/api", pr(1, "aaa"))
	reset := t0.Add(5 * time.Minute)
	f.limitUntil = reset

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &fakeClock{now: t0}
	clock.onSleep = func(n int) {
		switch n {
		case 1: // the limit lifts while we sleep
			f.mu.Lock()
			f.limitUntil = time.Time{}
			f.mu.Unlock()
		case 2:
			cancel()
		}
	}
	g := f.client(Options{Repos: []string{"acme/api", "acme/web"}, Poll: time.Minute}, clock)

	var got []Trigger
	err := g.Watch(ctx, func(tr Trigger) error { got = append(got, tr); return nil })
	if err != context.Canceled {
		t.Fatalf("Watch returned %v", err)
	}
	if clock.sleeps[0] != 5*time.Minute {
		t.Fatalf("first sleep = %v, want the 5m until reset", clock.sleeps[0])
	}
	if clock.sleeps[1] != time.Minute {
		t.Fatalf("after the reset the normal interval applies, got %v", clock.sleeps[1])
	}
	// The 429 on the first repo stopped the poll: the second repo was not asked,
	// and the PR is picked up once the limit lifted.
	eq(t, got, "push:aaa")
	if n := f.count("GET /repos/acme/web/pulls"); n != 1 {
		t.Fatalf("second repo polled %d times, want once (after the pause)", n)
	}
}

func TestWatchPlainForbiddenIsNotAPause(t *testing.T) {
	g := &GitHub{clock: &fakeClock{now: t0}}
	if rl := g.rateLimit(&response{status: 403, header: map[string][]string{}}); rl != nil {
		t.Fatalf("a 403 without rate-limit headers is a permission error, got pause until %v", rl.Reset)
	}
}

// A repository that lives only on this machine (a local checkout, no checks) has nothing on GitHub to poll.
func TestLocalOnlyRepositoriesAreNotPolled(t *testing.T) {
	o := OptionsFromConfig(&config.Config{Repos: []config.Repo{
		{Name: "acme/api", Checks: true},
		{Name: "acme/suites-only", Checks: false},
		{Name: "acme/local", Checks: false, Path: "/code/local"},
		{Name: "acme/mapped-with-checks", Checks: true, Path: "/code/mapped"},
	}}, "tok")
	if got := strings.Join(o.Repos, ","); got != "acme/api,acme/suites-only,acme/mapped-with-checks" {
		t.Fatalf("polled repos = %s", got)
	}
}

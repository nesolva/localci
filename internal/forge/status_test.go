package forge

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStatusDescriptions(t *testing.T) {
	since := time.Date(2026, 11, 19, 14, 2, 9, 0, time.UTC)
	long := strings.Repeat("é", 300)
	cases := []struct {
		name  string
		st    Status
		state State
		want  string
	}{
		{"queued", Queued("e2e", 3, ""), StatePending, "queued · 3 in line"},
		{"running", Running("e2e", 34, 128, since, ""), StatePending, "running · 34/128 · since 14:02"},
		{"passed", Passed("e2e", 128, 6*time.Minute+12*time.Second, ""), StateSuccess, "128 passed · 6m12s"},
		{"passed under a minute", Passed("e2e", 3, 45*time.Second, ""), StateSuccess, "3 passed · 45s"},
		{"failed", Failed("e2e", 2, "checkout › pays by card", ""), StateFailure, "2 failed: checkout › pays by card"},
		{"failed without a title", Failed("e2e", 1, "", ""), StateFailure, "1 failed"},
		{"multi-line title is one line", Failed("e2e", 1, "a\n  b", ""), StateFailure, "1 failed: a b"},
	}
	for _, c := range cases {
		if c.st.Description != c.want || c.st.State != c.state {
			t.Errorf("%s: got %q (%s), want %q (%s)", c.name, c.st.Description, c.st.State, c.want, c.state)
		}
	}
	got := Failed("e2e", 2, long, "").Description
	if n := len([]rune(got)); n != MaxDescription || !strings.HasSuffix(got, "…") {
		t.Errorf("long title: %d characters, want exactly %d ending in an ellipsis", n, MaxDescription)
	}
}

func TestSetStatusesPostsToPRHeadsOnly(t *testing.T) {
	f := newFake(t)
	g := f.client(Options{}, &fakeClock{now: t0})
	heads := []Head{
		{Repo: "acme/api", Branch: "feat", SHA: "aaa", PR: 4},
		{Repo: "acme/web", Branch: "feat", SHA: "bbb", PR: 9},
		{Repo: "acme/lib", Branch: "main", SHA: "ccc"}, // default branch: no PR
	}
	st := Passed("e2e", 128, 6*time.Minute+12*time.Second, "http://127.0.0.1:7070/runs/1")
	if err := SetStatuses(context.Background(), g, heads, st); err != nil {
		t.Fatal(err)
	}
	if len(f.statuses) != 2 {
		t.Fatalf("posted %d statuses, want 2", len(f.statuses))
	}
	s := f.statuses[0]
	if s["repo"] != "acme/api" || s["sha"] != "aaa" || s["state"] != "success" || s["context"] != "localci/e2e" ||
		s["target_url"] != "http://127.0.0.1:7070/runs/1" || s["description"] != "128 passed · 6m12s" {
		t.Fatalf("status body wrong: %v", s)
	}
}

func TestResolveHeadsPrefersSameBranchPRThenDefaultBranch(t *testing.T) {
	f := newFake(t)
	f.set("acme/api", fakePR{Number: 4, Updated: t0, Branch: "feat-x", SHA: "api-head", Base: "main"})
	// web has an open PR for the branch, but from a fork: it must not be used.
	f.set("acme/web", fakePR{Number: 9, Updated: t0, Branch: "feat-x", SHA: "web-fork", Base: "main", Fork: true})
	f.defaults["acme/web"] = [2]string{"trunk", "web-trunk"}
	f.set("acme/lib", fakePR{Number: 2, Updated: t0, Branch: "feat-x", SHA: "lib-head", Base: "main"})
	f.defaults["acme/lib"] = [2]string{"main", "lib-main"}
	f.set("acme/docs")
	f.defaults["acme/docs"] = [2]string{"main", "docs-main"}
	g := f.client(Options{}, &fakeClock{now: t0})

	trig := Trigger{Kind: KindPush, Repo: "acme/api", PR: 4, Branch: "feat-x", Head: "api-head", Base: "main"}
	got, err := g.ResolveHeads(context.Background(), []string{"acme/api", "acme/web", "acme/lib", "acme/docs"}, trig)
	if err != nil {
		t.Fatal(err)
	}
	want := []Head{
		{Repo: "acme/api", Branch: "feat-x", SHA: "api-head", PR: 4},
		{Repo: "acme/web", Branch: "trunk", SHA: "web-trunk"},
		{Repo: "acme/lib", Branch: "feat-x", SHA: "lib-head", PR: 2},
		{Repo: "acme/docs", Branch: "main", SHA: "docs-main"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("head %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A fork picks its own branch name: a branch of the same name in another repo
// of the suite is someone else's work, so it is neither tested nor reported on.
func TestAForkTriggerUsesTheDefaultBranchOfTheOtherRepos(t *testing.T) {
	f := newFake(t)
	f.set("acme/api", fakePR{Number: 4, Updated: t0, Branch: "feat-x", SHA: "fork-head", Base: "main", Fork: true})
	f.set("acme/lib", fakePR{Number: 2, Updated: t0, Branch: "feat-x", SHA: "lib-head", Base: "main"})
	f.defaults["acme/lib"] = [2]string{"main", "lib-main"}
	g := f.client(Options{Repos: []string{"acme/api"}, Forks: "always"}, &fakeClock{now: t0})
	trig := collect(g, 1, nil)
	eq(t, trig, "push:fork-head")

	heads, err := g.ResolveHeads(context.Background(), []string{"acme/api", "acme/lib"}, trig[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := (Head{Repo: "acme/lib", Branch: "main", SHA: "lib-main"}); len(heads) != 2 || heads[1] != want {
		t.Fatalf("heads = %+v, want lib at its default branch", heads)
	}
	if err := SetStatuses(context.Background(), g, heads, Queued("e2e", 0, "")); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.statuses {
		if s["repo"] != "acme/api" || s["sha"] != "fork-head" {
			t.Fatalf("status posted on %s@%s, only the fork's own PR may get one", s["repo"], s["sha"])
		}
	}
}

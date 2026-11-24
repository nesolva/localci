package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)

// fakeClock never really sleeps: Sleep advances time and tells the test.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(n int) // called after each Sleep with the count so far
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	n := len(c.sleeps)
	c.mu.Unlock()
	if c.onSleep != nil {
		c.onSleep(n)
	}
	return ctx.Err()
}

type fakePR struct {
	Number    int
	Draft     bool
	Updated   time.Time
	Branch    string
	SHA       string
	Base      string
	Author    string
	Labels    []string
	Fork      bool   // head lives in another repository
	HeadOwner string // owner of that repository; "stranger" when empty
}

// fakeReview is a pull request review; State is APPROVED, CHANGES_REQUESTED,
// COMMENTED or DISMISSED, Commit the head it was submitted on.
type fakeReview struct {
	User   string
	State  string
	Commit string
}

// fakeGitHub serves the few endpoints the forge uses, with real ETag semantics.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	pulls      map[string][]fakePR     // repo -> PRs
	etag       map[string]int          // repo -> version
	reviews    map[string][]fakeReview // "repo#n" -> reviews, oldest first
	perms      map[string]string       // login -> permission
	defaults   map[string][2]string    // repo -> {branch, sha}
	requests   []string                // "METHOD path" of every request
	statuses   []map[string]string
	limitUntil time.Time // while set, list calls answer 429
}

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, pulls: map[string][]fakePR{}, etag: map[string]int{},
		reviews: map[string][]fakeReview{}, perms: map[string]string{}, defaults: map[string][2]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) set(repo string, prs ...fakePR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[repo] = prs
	f.etag[repo]++
}

func (f *fakeGitHub) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) apiPull(repo string, p fakePR) map[string]any {
	var labels []map[string]string
	for _, l := range p.Labels {
		labels = append(labels, map[string]string{"name": l})
	}
	owner := strings.Split(repo, "/")[0]
	if p.Fork {
		owner = p.HeadOwner
		if owner == "" {
			owner = "stranger"
		}
	}
	headRepo := map[string]any{"full_name": owner + "/" + strings.Split(repo, "/")[1],
		"owner": map[string]string{"login": owner, "type": "User"}}
	return map[string]any{
		"number": p.Number, "draft": p.Draft, "updated_at": p.Updated, "labels": labels,
		"head": map[string]any{"ref": p.Branch, "sha": p.SHA, "repo": headRepo},
		"base": map[string]any{"ref": p.Base, "repo": map[string]string{"full_name": repo}},
		"user": map[string]string{"login": p.Author},
	}
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // repos o n …
	if len(parts) < 3 || parts[0] != "repos" {
		http.NotFound(w, r)
		return
	}
	repo := parts[1] + "/" + parts[2]
	rest := strings.Join(parts[3:], "/")
	switch {
	case r.Method == http.MethodGet && rest == "pulls":
		if !f.limitUntil.IsZero() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(f.limitUntil.Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if head := r.URL.Query().Get("head"); head != "" { // owner:branch filter
			var out []map[string]any
			for _, p := range f.pulls[repo] {
				if !p.Fork && parts[1]+":"+p.Branch == head {
					out = append(out, f.apiPull(repo, p))
				}
			}
			writeJSON(w, out)
			return
		}
		et := fmt.Sprintf(`"%s-%d"`, repo, f.etag[repo])
		if r.Header.Get("If-None-Match") == et {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", et)
		out := []map[string]any{}
		for _, p := range f.pulls[repo] {
			out = append(out, f.apiPull(repo, p))
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "pulls/") && strings.HasSuffix(rest, "/reviews"):
		n := strings.Split(rest, "/")[1]
		out := []map[string]any{}
		for i, rv := range f.reviews[repo+"#"+n] {
			out = append(out, map[string]any{"id": i + 1, "state": rv.State, "commit_id": rv.Commit,
				"user": map[string]string{"login": rv.User}})
		}
		writeJSON(w, out)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "collaborators/"):
		login := strings.Split(rest, "/")[1]
		perm, ok := f.perms[login]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"permission": perm})
	case r.Method == http.MethodGet && rest == "":
		writeJSON(w, map[string]string{"default_branch": f.defaults[repo][0]})
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "branches/"):
		writeJSON(w, map[string]any{"commit": map[string]string{"sha": f.defaults[repo][1]}})
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "statuses/"):
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["sha"] = strings.TrimPrefix(rest, "statuses/")
		body["repo"] = repo
		f.statuses = append(f.statuses, body)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, body)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) client(o Options, clock Clock) *GitHub {
	o.BaseURL = f.srv.URL
	o.Clock = clock
	if o.Poll == 0 {
		o.Poll = time.Minute
	}
	return NewGitHub(o)
}

// collect polls g `times` times against the fake and returns what was emitted.
func collect(g *GitHub, times int, between func(i int)) []Trigger {
	var got []Trigger
	for i := 0; i < times; i++ {
		g.pollOnce(context.Background(), func(t Trigger) error { got = append(got, t); return nil })
		if between != nil {
			between(i)
		}
	}
	return got
}

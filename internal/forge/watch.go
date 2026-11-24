package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type apiPull struct {
	Number    int       `json:"number"`
	Draft     bool      `json:"draft"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
			Owner    struct {
				Login string `json:"login"`
				Type  string `json:"type"` // User or Organization
			} `json:"owner"`
		} `json:"repo"` // null when the fork was deleted
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (p *apiPull) isFork() bool {
	return p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, p.Base.Repo.FullName)
}

// headOwnedByAuthor reports whether the head branch lives in a repository the
// author owns as a user. Anyone who can push to the head repository can change
// the PR's code, so only then is the author the one who controls it (short of
// collaborators the author added to that fork).
func (p *apiPull) headOwnedByAuthor() bool {
	r := p.Head.Repo
	return r != nil && p.User.Login != "" && strings.EqualFold(r.Owner.Type, "User") &&
		strings.EqualFold(r.Owner.Login, p.User.Login)
}

func (p *apiPull) hasLabel(name string) bool {
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, name) {
			return true
		}
	}
	return false
}

// prState is what the watcher remembers about one open PR.
type prState struct {
	emittedHead  string // last head a push trigger was handled for
	tryHead      string // head whose handler answered ErrNotReady ...
	tries        int    // ... this many times so far
	labelSeen    bool   // the trigger label was present at the last look
	approvedHead string // fork PR: the head a writer's review approved (a cache only)
}

// Watch polls every configured repo each poll interval and emits triggers until
// ctx is done. It sleeps through rate limits instead of failing.
func (g *GitHub) Watch(ctx context.Context, emit func(Trigger) error) error {
	for {
		pause := g.pollOnce(ctx, emit)
		wait := g.o.Poll
		if d := pause.Sub(g.clock.Now()); d > wait {
			wait = d
		}
		if err := g.clock.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// pollOnce polls every repo once. It returns the time polling must resume at
// (zero when no rate limit was hit).
func (g *GitHub) pollOnce(ctx context.Context, emit func(Trigger) error) (resume time.Time) {
	for _, repo := range g.o.Repos {
		if ctx.Err() != nil {
			return
		}
		err := g.pollRepo(ctx, repo, emit)
		var rl *RateLimitError
		switch {
		case errors.As(err, &rl):
			return rl.Reset // the limit is per token: skip the other repos too
		case err != nil && ctx.Err() == nil && g.o.OnError != nil:
			g.o.OnError(err)
		}
	}
	return
}

func (g *GitHub) pollRepo(ctx context.Context, repo string, emit func(Trigger) error) error {
	// Sorted by update time, newest first: the first page holds every PR young
	// enough to matter, and one ETag covers it (a 304 costs no rate limit).
	q := url.Values{"state": {"open"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}}
	hdr := map[string]string{}
	if et := g.etags[repo]; et != "" {
		hdr["If-None-Match"] = et
	}
	r, err := g.do(ctx, http.MethodGet, repoPath(repo)+"/pulls?"+q.Encode(), nil, hdr)
	if err != nil {
		return err
	}
	switch r.status {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return r.err("list PRs of " + repo)
	}
	var pulls []apiPull
	if err := json.Unmarshal(r.body, &pulls); err != nil {
		return err
	}
	// Keep the old ETag when a PR could not be handled, so the next poll
	// fetches the list again instead of getting a 304.
	if err := g.handlePulls(ctx, repo, pulls, emit); errors.Is(err, errPollAgain) {
		return nil
	} else if err != nil {
		return err
	}
	g.etags[repo] = r.header.Get("ETag")
	return nil
}

func (g *GitHub) handlePulls(ctx context.Context, repo string, pulls []apiPull, emit func(Trigger) error) error {
	now := g.clock.Now()
	present := map[string]bool{}
	var firstErr error
	notReady := false
	for i := range pulls {
		p := &pulls[i]
		key := repo + "#" + strconv.Itoa(p.Number)
		present[key] = true
		if p.Draft && !g.o.Drafts {
			continue
		}
		if g.o.MaxAge > 0 && now.Sub(p.UpdatedAt) > g.o.MaxAge {
			continue
		}
		st := g.prs[key]
		if st == nil {
			st = &prState{}
			g.prs[key] = st
		}
		ok, err := g.allowed(ctx, repo, p, st)
		if err != nil {
			if rl := (*RateLimitError)(nil); errors.As(err, &rl) {
				return err
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ok {
			continue
		}
		t := Trigger{Kind: KindPush, Repo: repo, PR: p.Number, Branch: p.Head.Ref, Head: p.Head.SHA,
			Base: p.Base.Ref, Author: p.User.Login, Fork: p.isFork()}
		if st.emittedHead != p.Head.SHA {
			if st.tryHead != p.Head.SHA {
				st.tryHead, st.tries = p.Head.SHA, 0
			}
			st.tries++
			push := t
			push.Attempt, push.LastAttempt = st.tries, st.tries >= MaxAttempts
			if err := emit(push); errors.Is(err, ErrNotReady) && !push.LastAttempt {
				notReady = true // not consumed: the next poll emits this head again
			} else {
				st.emittedHead, st.tryHead, st.tries = p.Head.SHA, "", 0
			}
		}
		if has := p.hasLabel(g.o.Label); has && !st.labelSeen {
			st.labelSeen = true
			t.Kind, t.Label = KindLabel, g.o.Label
			_ = emit(t)
		} else if !has {
			st.labelSeen = false
		}
	}
	for key := range g.prs {
		if strings.HasPrefix(key, repo+"#") && !present[key] {
			delete(g.prs, key) // closed or merged
		}
	}
	if firstErr == nil && notReady {
		return errPollAgain
	}
	return firstErr
}

// errPollAgain means every PR was handled except heads whose handler was not
// ready: nothing to report, but the list must be fetched again next time.
var errPollAgain = errors.New("poll again")

// allowed applies the fork policy. A PR from the repo itself always passes.
//
// Under `label` a fork PR runs while it carries the OK label and a user with
// write access has approved its current head in a review, or its author has
// write access and owns the fork its head lives in (GitHub forbids approving
// your own PR). A writer's PR from someone else's fork needs the review: that
// fork's owner can push to the head. GitHub records the
// commit each review was submitted on, so the approval covers exactly the code
// the writer saw: any later push (also one made before localci first looks,
// while localci was down, or across a close and reopen) needs a new approval.
// Removing the label revokes it. Nothing is approved from memory alone.
func (g *GitHub) allowed(ctx context.Context, repo string, p *apiPull, st *prState) (bool, error) {
	if !p.isFork() {
		return true, nil
	}
	switch g.o.Forks {
	case "always":
		return true, nil
	case "label":
	default: // never
		return false, nil
	}
	if !p.hasLabel(g.o.OKLabel) {
		st.approvedHead = "" // label removed: approval is gone
		return false, nil
	}
	if p.Head.SHA != "" && st.approvedHead == p.Head.SHA {
		return true, nil // checked against GitHub for this very head before
	}
	st.approvedHead = ""
	// GitHub forbids approving one's own pull request, so a writer's PR from
	// their own fork counts as approved by its author for whatever head it has.
	var ok bool
	var err error
	if p.headOwnedByAuthor() {
		ok, err = g.canWrite(ctx, repo, p.User.Login)
	}
	if err == nil && !ok {
		ok, err = g.approvedByWriter(ctx, repo, p.Number, p.Head.SHA)
	}
	if err != nil || !ok || p.Head.SHA == "" {
		return false, err
	}
	st.approvedHead = p.Head.SHA
	return true, nil
}

// approvedByWriter reports whether a user with write access approved pull
// request pr in a review submitted on commit sha. Only each reviewer's latest
// approving, changes-requested or dismissed review counts.
func (g *GitHub) approvedByWriter(ctx context.Context, repo string, pr int, sha string) (bool, error) {
	if sha == "" {
		return false, nil
	}
	type review struct{ state, commit string }
	latest := map[string]review{} // reviewer -> latest decisive review
	var order []string
	err := g.getAll(ctx, repoPath(repo)+"/pulls/"+strconv.Itoa(pr)+"/reviews?per_page=100", 30, func(raw json.RawMessage) error {
		var rv struct {
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			User     *struct {
				Login string `json:"login"`
			} `json:"user"` // null for a deleted account
		}
		if err := json.Unmarshal(raw, &rv); err != nil {
			return err
		}
		switch rv.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		default: // COMMENTED and PENDING decide nothing
			return nil
		}
		if rv.User == nil || rv.User.Login == "" {
			return nil
		}
		if _, seen := latest[rv.User.Login]; !seen {
			order = append(order, rv.User.Login)
		}
		latest[rv.User.Login] = review{rv.State, rv.CommitID} // GitHub lists them oldest first
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, login := range order {
		if rv := latest[login]; rv.state != "APPROVED" || rv.commit != sha {
			continue
		}
		writer, err := g.canWrite(ctx, repo, login)
		if err != nil {
			return false, err
		}
		if writer {
			return true, nil
		}
	}
	return false, nil
}

// canWrite reports whether login has write access (or more) to repo.
func (g *GitHub) canWrite(ctx context.Context, repo, login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	r, err := g.do(ctx, http.MethodGet, repoPath(repo)+"/collaborators/"+url.PathEscape(login)+"/permission", nil, nil)
	if err != nil {
		return false, err
	}
	switch r.status {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	default:
		return false, r.err("get permission")
	}
	var perm struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if err := json.Unmarshal(r.body, &perm); err != nil {
		return false, err
	}
	switch perm.Permission {
	case "admin", "write":
		return true, nil
	}
	return perm.RoleName == "maintain", nil
}

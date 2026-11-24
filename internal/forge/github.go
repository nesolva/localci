package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/config"
)

// Clock is time as the watcher sees it; tests inject a fake.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Options configures GitHub.
type Options struct {
	Token      string // from ResolveToken
	BaseURL    string // default https://api.github.com
	HTTPClient *http.Client
	Clock      Clock

	Repos   []string      // owner/name to watch
	Poll    time.Duration // between polls
	MaxAge  time.Duration // ignore PRs not updated for this long (0 = no limit)
	Drafts  bool          // also trigger on draft PRs
	Forks   string        // never | label | always
	Label   string        // label that triggers a run (default "e2e")
	OKLabel string        // label that approves a fork PR (default "ok-to-test")

	// OnError receives errors of a poll (the watcher carries on).
	OnError func(error)
}

// OptionsFromConfig fills the watch options from the effective configuration.
func OptionsFromConfig(c *config.Config, token string) Options {
	o := Options{Token: token, Poll: c.Watch.Poll, MaxAge: c.Watch.MaxAge, Drafts: c.Watch.Drafts, Forks: c.Watch.Forks}
	for _, r := range c.Repos {
		o.Repos = append(o.Repos, r.Name)
	}
	return o
}

// ResolveToken returns $GITHUB_TOKEN, else the output of `gh auth token`.
func ResolveToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, nil
		}
	}
	return "", errors.New("no GitHub token: set GITHUB_TOKEN or run `gh auth login`")
}

// GitHub is the GitHub REST implementation of Forge.
type GitHub struct {
	o     Options
	hc    *http.Client
	clock Clock
	base  string

	// Watch state; only the goroutine running Watch touches it.
	etags map[string]string
	prs   map[string]*prState
}

var _ Forge = (*GitHub)(nil)

// NewGitHub builds a client; zero Options fields take their defaults.
func NewGitHub(o Options) *GitHub {
	g := &GitHub{o: o, hc: o.HTTPClient, clock: o.Clock, base: strings.TrimRight(o.BaseURL, "/"),
		etags: map[string]string{}, prs: map[string]*prState{}}
	if g.hc == nil {
		g.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if g.clock == nil {
		g.clock = realClock{}
	}
	if g.base == "" {
		g.base = "https://api.github.com"
	}
	if g.o.Poll <= 0 {
		g.o.Poll = time.Minute
	}
	if g.o.Label == "" {
		g.o.Label = "e2e"
	}
	if g.o.OKLabel == "" {
		g.o.OKLabel = "ok-to-test"
	}
	if g.o.Forks == "" {
		g.o.Forks = "label"
	}
	return g
}

// RateLimitError means GitHub asked us to stop until Reset.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "GitHub rate limit reached, resumes at " + e.Reset.Format(time.RFC3339)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request; target is a path ("/repos/…") or an absolute URL on
// the API's own scheme and host (the token is never sent anywhere else).
func (g *GitHub) do(ctx context.Context, method, target string, payload any, hdr map[string]string) (*response, error) {
	if strings.HasPrefix(target, "/") {
		target = g.base + target
	} else if !sameOrigin(target, g.base) {
		return nil, fmt.Errorf("refusing to send the GitHub token to %q: not on %s", target, g.base)
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "localci")
	if g.o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.o.Token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	r := &response{status: resp.StatusCode, header: resp.Header, body: b}
	if rl := g.rateLimit(r); rl != nil {
		return nil, rl
	}
	return r, nil
}

// rateLimit recognises a 403/429 that is a rate limit (not a permission error).
func (g *GitHub) rateLimit(r *response) *RateLimitError {
	if r.status != http.StatusForbidden && r.status != http.StatusTooManyRequests {
		return nil
	}
	now := g.clock.Now()
	var reset time.Time
	if ra := r.header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil {
			reset = now.Add(time.Duration(s) * time.Second)
		}
	} else if r.header.Get("X-RateLimit-Remaining") == "0" {
		if s, err := strconv.ParseInt(r.header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			reset = time.Unix(s, 0)
		}
	}
	if reset.IsZero() {
		if r.status == http.StatusTooManyRequests {
			reset = now.Add(time.Minute) // limited, but no reset given
		} else {
			return nil
		}
	}
	if reset.Before(now) {
		reset = now.Add(time.Second)
	}
	return &RateLimitError{Reset: reset}
}

func (r *response) err(what string) error {
	msg := strings.TrimSpace(string(r.body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("%s: HTTP %d %s", what, r.status, msg)
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getAll GETs a list endpoint, following rel="next" up to maxPages pages.
func (g *GitHub) getAll(ctx context.Context, path string, maxPages int, each func(raw json.RawMessage) error) error {
	target := path
	for i := 0; target != "" && i < maxPages; i++ {
		r, err := g.do(ctx, http.MethodGet, target, nil, nil)
		if err != nil {
			return err
		}
		if r.status != http.StatusOK {
			return r.err("GET " + path)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(r.body, &items); err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return err
			}
		}
		target = ""
		if m := nextLink.FindStringSubmatch(r.header.Get("Link")); m != nil {
			target = m[1]
		}
	}
	return nil
}

// sameOrigin reports whether URLs a and b have the same scheme and host:port.
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host != "" &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func repoPath(repo string) string {
	o, n, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(o) + "/" + url.PathEscape(n)
}

// SetStatus posts a commit status.
func (g *GitHub) SetStatus(ctx context.Context, repo, sha string, st Status) error {
	body := map[string]string{
		"state":       string(st.State),
		"context":     Context(st.Suite),
		"description": truncate(st.Description),
	}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	r, err := g.do(ctx, http.MethodPost, repoPath(repo)+"/statuses/"+url.PathEscape(sha), body, nil)
	if err != nil {
		return err
	}
	if r.status != http.StatusCreated {
		return r.err("POST status")
	}
	return nil
}

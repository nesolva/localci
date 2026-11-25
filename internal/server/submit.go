package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

const requestedBy = "cli"

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "the body is JSON like {\"suite\":\"e2e\"}: %v", err)
		return
	}
	var res SubmitResult
	var err error
	if req.Checks {
		res, err = s.submitChecks(r.Context(), req)
	} else {
		res, err = s.submitSuite(r.Context(), req)
	}
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			fail(w, he.status, "%s", he.msg)
		} else {
			fail(w, http.StatusInternalServerError, "%v", err)
		}
		return
	}
	res.Cursor = s.lastSeq(r.Context())
	writeJSON(w, http.StatusAccepted, res)
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func bad(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) error {
	return &httpError{http.StatusConflict, fmt.Sprintf(format, args...)}
}

func (s *Server) submitSuite(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if s.d.Suites == nil {
		return SubmitResult{}, conflict("no suites are configured: add them under `suites:` in the config file")
	}
	name := req.Suite
	if name == "" {
		names := s.d.Suites.Names()
		switch len(names) {
		case 0:
			return SubmitResult{}, conflict("no suites are configured: add them under `suites:` in the config file")
		case 1:
			name = names[0]
		default:
			return SubmitResult{}, bad("which suite? use --suite with one of: %s", strings.Join(names, ", "))
		}
	}
	sd, ok := s.d.Suites.Suite(name)
	if !ok {
		return SubmitResult{}, bad("unknown suite %q (configured: %s)", name, strings.Join(s.d.Suites.Names(), ", "))
	}
	sr := suite.Request{Suite: name, Trigger: "manual", RequestedBy: requestedBy,
		Tests: suite.Filter{Grep: req.Grep, Files: req.Files, FailedOf: req.Failed}}
	if req.Affected {
		sr.Affected = &suite.AffectedRequest{Base: req.Base, Explain: req.Explain}
	}
	if req.Failed != "" {
		if _, err := s.d.Store.Run(ctx, req.Failed); err != nil {
			return SubmitResult{}, bad("no run %s to take failed tests from", req.Failed)
		}
	}
	if req.Dir != "" {
		heads, err := suiteHeads(ctx, sd, req.Dir)
		if err != nil {
			return SubmitResult{}, bad("%v", err)
		}
		sr.Heads = heads
	}
	id, err := s.d.Suites.Submit(sr)
	if err != nil {
		return SubmitResult{}, bad("%v", err)
	}
	run, err := s.d.Store.Run(ctx, id)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Runs: []Run{s.summarize(ctx, run)}}, nil
}

// suiteHeads is the working tree of dir as the head of its repository in the suite. A directory that is not one of
// the suite's repositories is an error: running the default branch instead would test something else than what the
// caller has in front of them, without saying so.
func suiteHeads(ctx context.Context, sd *suite.Suite, dir string) ([]suite.Head, error) {
	co, err := checkoutOf(ctx, dir)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(sd.Repos, co.repo) {
		return nil, fmt.Errorf("%s is %s, which suite %q does not test (its repos: %s): run from one of them",
			co.top, co.repo, sd.Name, strings.Join(sd.Repos, ", "))
	}
	h := suite.Head{Dir: co.top, Dirty: co.dirty}
	h.Repo, h.Branch, h.SHA = co.repo, co.branch, co.sha
	return []suite.Head{h}, nil
}

func (s *Server) submitChecks(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if s.d.Checks == nil {
		return SubmitResult{}, conflict("checks are not available on this server")
	}
	if req.Dir == "" {
		return SubmitResult{}, bad("--checks needs the checkout to test: run it inside the repository")
	}
	co, err := checkoutOf(ctx, req.Dir)
	if err != nil {
		return SubmitResult{}, bad("%v", err)
	}
	snap, err := s.d.Checks.Snapshot(ctx, co.repo, co.top)
	if err != nil {
		return SubmitResult{}, err
	}
	sub, err := s.d.Checks.Submit(ctx, checks.Request{Repo: co.repo, Snapshot: snap.SHA, Trigger: "manual",
		RequestedBy: requestedBy, Force: req.Force})
	if err != nil {
		return SubmitResult{}, err
	}
	res := SubmitResult{Runs: []Run{}}
	for _, st := range sub.Runs {
		run, err := s.d.Store.Run(ctx, st.RunID)
		if err != nil {
			return res, err
		}
		res.Runs = append(res.Runs, s.summarize(ctx, run))
	}
	for _, sk := range sub.Skipped {
		res.Skipped = append(res.Skipped, Skipped{Workflow: sk.Workflow, Job: sk.Job, Reason: sk.Reason})
	}
	return res, nil
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !store.Terminal(run.State) {
		fail(w, http.StatusConflict, "run %s is still %s", run.ID, run.State)
		return
	}
	if run.Trigger == suite.ExploreTrigger {
		fail(w, http.StatusConflict, "run %s was an exploratory run: start another with `localci explore`", run.ID)
		return
	}
	var res SubmitResult
	var err error
	if run.Kind == store.KindCheck {
		res, err = s.retryCheck(ctx, run)
	} else {
		res, err = s.retrySuite(ctx, run)
	}
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			fail(w, he.status, "%s", he.msg)
		} else {
			fail(w, http.StatusInternalServerError, "%v", err)
		}
		return
	}
	res.Cursor = s.lastSeq(ctx)
	writeJSON(w, http.StatusAccepted, res)
}

func (s *Server) retrySuite(ctx context.Context, run store.Run) (SubmitResult, error) {
	if s.d.Suites == nil {
		return SubmitResult{}, conflict("no suites are configured")
	}
	var heads []suite.Head
	json.Unmarshal(run.Heads, &heads)
	sr := suite.Request{Suite: run.Suite, Heads: heads, Trigger: "retry", RequestedBy: requestedBy, Fork: run.Fork}
	if run.Affected {
		// The retry asks for the same diff.
		sr.Affected = &suite.AffectedRequest{Base: s.affectedBase(ctx, run.ID)}
	}
	// Read the cursor before Submit starts the job: a run that ends fast must not slip between the two.
	since := s.lastSeq(ctx)
	id, err := s.d.Suites.Submit(sr)
	if err != nil {
		return SubmitResult{}, bad("%v", err)
	}
	// A run with pull request heads reports under its own context, affected or not: a queued run the retry
	// supersedes leaves its status to the run that supersedes it, so a retry that stayed silent would leave
	// `localci/<suite>` pending for ever.
	if s.d.Report != nil && slices.ContainsFunc(heads, func(h suite.Head) bool { return h.PR > 0 }) {
		var hs []forge.Head
		for _, h := range heads {
			hs = append(hs, h.Head)
		}
		s.d.Report(id, suite.StatusName(run.Suite, run.Affected), hs, since)
	}
	next, err := s.d.Store.Run(ctx, id)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Runs: []Run{s.summarize(ctx, next)}}, nil
}

// affectedBase is the branch the diff of an affected run started from, as its plan recorded it ("" = default).
func (s *Server) affectedBase(ctx context.Context, run string) string {
	ev, ok, _ := s.d.Store.FirstEvent(ctx, run, "run.affected")
	if !ok {
		return ""
	}
	var d struct{ Base string }
	json.Unmarshal(ev.Data, &d)
	return d.Base
}

func (s *Server) retryCheck(ctx context.Context, run store.Run) (SubmitResult, error) {
	if s.d.Checks == nil {
		return SubmitResult{}, conflict("checks are not available on this server")
	}
	var heads []Head
	json.Unmarshal(run.Heads, &heads)
	if len(heads) != 1 || heads[0].PR == 0 {
		return SubmitResult{}, bad("a check of a working tree cannot be retried: run `localci run --checks` again")
	}
	h := heads[0]
	// Only the check being retried starts, and only its status is posted. Submit still plans and names every
	// check of the pull request first, so a name that carries a matrix suffix is found.
	want := forge.Context(run.Suite)
	sub, err := s.d.Checks.Submit(ctx, checks.Request{Repo: h.Repo, PR: h.PR, Base: h.Branch, Head: h.SHA, Trigger: "manual",
		RequestedBy: requestedBy, Force: true, Only: want})
	var pe *checks.PullRefError
	if errors.As(err, &pe) {
		if pe.MergeRef() {
			return SubmitResult{}, conflict("GitHub has not rebuilt the merge ref of the pull request yet: retry in a moment")
		}
		return SubmitResult{}, conflict("the pull request moved to %s: retry from there", shortSHA(pe.Got))
	}
	if err != nil {
		return SubmitResult{}, err
	}
	if len(sub.Runs) == 0 {
		return SubmitResult{}, conflict("%s is not among the checks of %s#%d at %s any more: the workflow changed", want, h.Repo, h.PR, shortSHA(h.SHA))
	}
	res := SubmitResult{Runs: []Run{}}
	for _, st := range sub.Runs {
		next, err := s.d.Store.Run(ctx, st.RunID)
		if err != nil {
			return res, err
		}
		res.Runs = append(res.Runs, s.summarize(ctx, next))
	}
	return res, nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// checkout is what the server needs to know about a directory it was asked to test.
type checkout struct {
	top, repo, branch, sha string
	dirty                  bool
}

// remoteRepo takes owner/name from the end of a remote URL, whatever its host: an SSH alias such as
// git@github-work:acme/web.git has none that says "github".
var remoteRepo = regexp.MustCompile(`[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

func checkoutOf(ctx context.Context, dir string) (checkout, error) {
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	var c checkout
	var err error
	if c.top, err = git("rev-parse", "--show-toplevel"); err != nil {
		return c, fmt.Errorf("%s is not inside a git repository", dir)
	}
	// config, not `remote get-url`: the latter applies url.<base>.insteadOf, which can hide the repository's name.
	url, err := git("config", "--get", "remote.origin.url")
	if err != nil || url == "" {
		return c, fmt.Errorf("%s has no `origin` remote, so its GitHub repository is unknown", c.top)
	}
	m := remoteRepo.FindStringSubmatch(url)
	if m == nil {
		return c, fmt.Errorf("`origin` of %s (%s) is not an owner/name repository address", c.top, url)
	}
	c.repo = m[1]
	c.sha, _ = git("rev-parse", "HEAD")
	c.branch, _ = git("rev-parse", "--abbrev-ref", "HEAD")
	st, _ := git("status", "--porcelain", "--untracked-files=no")
	c.dirty = st != ""
	return c, nil
}

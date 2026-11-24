package affected

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Planner is the suite runner's Affector: it holds the coverage map in the store, chooses tests from it and
// refreshes it from finished runs.
type Planner struct {
	Store *store.Store

	mu   sync.Mutex
	cfg  map[string]Config
	runs map[string]map[string]*measured // run → test → backend measurements
}

type measured struct {
	files      map[string]bool
	failed     bool // a reset or dump did not work: what was measured is partial
	incomplete bool // a file that ran could not be placed in the repository: the set has a hole
}

// Register installs p on the runner: the affected choice, and the hooks around every test that collect the
// backend coverage the suites configure. It fails on a suite whose coverage keys are wrong.
func (p *Planner) Register(r *suite.Runner) error {
	p.cfg, p.runs = map[string]Config{}, map[string]map[string]*measured{}
	for _, name := range r.Names() {
		s, _ := r.Suite(name)
		c, err := ParseConfig(s)
		if err != nil {
			return err
		}
		p.cfg[name] = c
	}
	r.SetAffector(p)
	r.OnTest(p.begin, p.end)
	return nil
}

const maxFiles, maxPath = 5000, 500

// Plan implements suite.Affector.
func (p *Planner) Plan(ctx context.Context, in suite.PlanInput) (suite.Plan, error) {
	s := in.Suite
	cfg := p.cfg[s.Name]
	var changed []string
	bases := map[string]string{}
	for _, repo := range s.Repos {
		st, ok := in.Repos[repo]
		if !ok || st.Mirror == nil || st.Head == "" {
			return suite.Plan{}, fmt.Errorf("repository %s is not checked out", repo)
		}
		base, err := resolveBase(ctx, st.Mirror, in.Request.Base)
		if err != nil {
			return suite.Plan{}, fmt.Errorf("%s: %w", repo, err)
		}
		bases[repo] = base
		mb, err := git(ctx, st.Mirror.Dir, "merge-base", base, st.Head)
		if err != nil {
			return suite.Plan{}, fmt.Errorf("%s: %w", repo, err)
		}
		out, err := git(ctx, st.Mirror.Dir, "diff", "--name-only", "--no-renames", "-z", mb, st.Head)
		if err != nil {
			return suite.Plan{}, fmt.Errorf("%s: %w", repo, err)
		}
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				changed = append(changed, repo+":"+f)
			}
		}
	}
	slices.Sort(changed)

	entries, err := p.Store.CoverageMap(ctx, s.Name)
	if err != nil {
		return suite.Plan{}, err
	}
	sel := Select(Input{Changed: changed, Tests: tests(in.Tests), TestRepo: s.Test.Repo, Map: filesOf(entries),
		AgeCommits: age(ctx, entries, in.Repos, bases), AlwaysAll: alwaysAll(s, cfg)})
	detail, _ := json.Marshal(sel)
	return suite.Plan{All: sel.All, IDs: sel.IDs(), Detail: detail}, nil
}

func tests(l []suite.TestRef) []Test {
	out := make([]Test, len(l))
	for i, t := range l {
		out[i] = Test{ID: t.ID, File: t.File, Title: t.Title}
	}
	return out
}

func filesOf(m map[string]store.CoverageEntry) map[string][]string {
	out := make(map[string][]string, len(m))
	for id, e := range m {
		out[id] = e.Files
	}
	return out
}

// alwaysAll adds the suite's own patterns and its prepare cache keys (a change there changes what is installed).
func alwaysAll(s *suite.Suite, c Config) []string {
	out := slices.Clone(c.AlwaysAll)
	for _, p := range s.Prepare {
		for _, k := range p.CacheKey {
			out = append(out, p.Repo+":"+k)
		}
	}
	return out
}

// resolveBase is the commit the diff starts from: the named branch, else the mirror's default branch. A
// repository without the named branch falls back to its default.
func resolveBase(ctx context.Context, m *exec.Mirror, branch string) (string, error) {
	if branch != "" {
		if sha, err := m.FetchBranch(ctx, branch); err == nil {
			return sha, nil
		}
	}
	if err := m.Fetch(ctx); err != nil {
		return "", err
	}
	return git(ctx, m.Dir, "rev-parse", "--verify", "HEAD^{commit}")
}

// age is how many commits on the base the oldest measurement is behind; -1 when a commit cannot be found.
func age(ctx context.Context, entries map[string]store.CoverageEntry, repos map[string]suite.RepoState, bases map[string]string) int {
	seen := map[string]bool{}
	worst := 0
	for _, e := range entries {
		for repo, sha := range e.Commits {
			key := repo + "@" + sha
			st, ok := repos[repo]
			if seen[key] {
				continue
			}
			seen[key] = true
			if !ok || st.Mirror == nil || bases[repo] == "" {
				continue // a repository the suite dropped does not age the map
			}
			out, err := git(ctx, st.Mirror.Dir, "rev-list", "--count", sha+".."+bases[repo])
			n, err2 := strconv.Atoi(out)
			if err != nil || err2 != nil {
				return -1
			}
			worst = max(worst, n)
		}
	}
	return worst
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// Done implements suite.Affector: it refreshes the map entries of the tests that passed. A fork's report is
// untrusted (the code under test writes it), so it teaches nothing: an approved fork could otherwise empty a
// test's set and make later runs skip it.
func (p *Planner) Done(ctx context.Context, in suite.DoneInput) {
	p.mu.Lock()
	backend := p.runs[in.Run]
	delete(p.runs, in.Run)
	p.mu.Unlock()
	if in.Fork {
		return
	}
	cfg := p.cfg[in.Suite.Name]
	res := newResolver(ctx, in.Suite, in.Repos)
	ran := map[string]bool{}
	for _, t := range in.Ran {
		ran[t.ID] = true
	}
	commits := map[string]string{}
	for repo, st := range in.Repos {
		commits[repo] = st.Head
	}
	// Only a run of the default branch says what main reaches; any other run may only add to it.
	merge := !onDefaultBranch(ctx, in.Repos)
	var entries []store.CoverageEntry
	var holes []string // tests whose coverage is only partly understood
	for _, id := range in.Passed {
		if !ran[id] {
			continue
		}
		files := map[string]bool{}
		known := false
		// The package says part of what the test reached could not be attributed, whether or not it named any file.
		hole := in.Incomplete[id]
		// An empty report says nothing about what the test reaches (coverage that did not work looks the same): it
		// must not become a known entry with no files.
		if reported := in.Frontend[id]; len(reported) > 0 {
			known = true
			// A source the package could not attribute, or one the repositories do not track, is a hole in the set:
			// a change there would select nothing. The test keeps running every time instead.
			for _, f := range reported {
				q, ok := res.resolve(f)
				if !ok {
					hole = true
					break
				}
				files[q] = true
			}
		}
		if hole {
			holes = append(holes, id)
			continue
		}
		if len(cfg.Backends) > 0 {
			m := backend[id]
			if m == nil || m.failed || m.incomplete {
				holes = append(holes, id)
				continue
			}
			known = true
			for f := range m.files {
				files[f] = true
			}
		}
		if !known {
			continue
		}
		list := make([]string, 0, len(files))
		for f := range files {
			list = append(list, f)
		}
		slices.Sort(list)
		// Too many files to be a choice, or one too long to keep: a set that is cut would select nothing for a
		// change in what was cut, so the test keeps running instead.
		if len(list) > maxFiles || slices.ContainsFunc(list, func(f string) bool { return len(f) > maxPath }) {
			holes = append(holes, id)
			continue
		}
		entries = append(entries, store.CoverageEntry{Test: id, Files: list, Commits: commits, Merge: merge})
	}
	var keep []string
	if in.Listed != nil {
		keep = []string{}
		for _, t := range in.Listed {
			keep = append(keep, t.ID)
		}
	}
	// What the default branch reaches is now only partly known for these tests: an older, complete-looking entry
	// would skip them for a change in the part that is not. Other runs only add, so they leave the entry alone.
	if !merge && len(holes) > 0 {
		p.Store.DropCoverage(ctx, in.Suite.Name, holes)
	}
	if len(entries) == 0 && keep == nil {
		return
	}
	p.Store.PutCoverage(ctx, in.Suite.Name, entries, keep)
}

// onDefaultBranch says whether every repository of the run was tested at a commit the mirror's default branch
// already holds. What cannot be told counts as not on it: adding files is the safe way to be wrong.
func onDefaultBranch(ctx context.Context, repos map[string]suite.RepoState) bool {
	if len(repos) == 0 {
		return false
	}
	for _, st := range repos {
		if st.Mirror == nil || st.Head == "" {
			return false
		}
		if _, err := git(ctx, st.Mirror.Dir, "merge-base", "--is-ancestor", st.Head, "HEAD"); err != nil {
			return false
		}
	}
	return true
}

// resolver qualifies the files a report names with the repository that tracks them.
type resolver struct {
	repos   []string // the test repository first
	dirs    map[string]string
	tracked map[string][]string
	set     map[string]map[string]bool
}

func newResolver(ctx context.Context, s *suite.Suite, st map[string]suite.RepoState) *resolver {
	r := &resolver{dirs: map[string]string{}, tracked: map[string][]string{}, set: map[string]map[string]bool{}}
	r.repos = append([]string{s.Test.Repo}, slices.DeleteFunc(slices.Clone(s.Repos), func(x string) bool { return x == s.Test.Repo })...)
	for _, repo := range r.repos {
		r.dirs[repo] = st[repo].Dir
		if r.dirs[repo] == "" {
			continue
		}
		out, err := git(ctx, r.dirs[repo], "ls-files", "-z")
		if err != nil {
			continue
		}
		r.set[repo] = map[string]bool{}
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				r.tracked[repo] = append(r.tracked[repo], f)
				r.set[repo][f] = true
			}
		}
	}
	return r
}

func (r *resolver) resolve(p string) (string, bool) {
	if len(p) > maxPath {
		return "", false
	}
	if repo, rest, ok := strings.Cut(p, ":"); ok && slices.Contains(r.repos, repo) && r.set[repo][rest] {
		return p, true
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "file://") {
		for _, repo := range r.repos {
			if rel, ok := relative(r.dirs[repo], p); ok && r.set[repo][rel] {
				return repo + ":" + rel, true
			}
		}
		return "", false
	}
	p = strings.TrimLeft(p, "./")
	for strings.HasPrefix(p, "../") {
		p = strings.TrimPrefix(p, "../")
	}
	for _, repo := range r.repos {
		if r.set[repo][p] {
			return repo + ":" + p, true
		}
	}
	// The bundler may have named the file relative to the app, not to the checkout: one tracked file ending
	// with it is that file.
	var found string
	n := 0
	for _, repo := range r.repos {
		for _, f := range r.tracked[repo] {
			if strings.HasSuffix(f, "/"+p) {
				found, n = repo+":"+f, n+1
			}
		}
	}
	return found, n == 1
}

// begin and end run around every test, blocking it: they reset and dump the configured backend coverage.
func (p *Planner) begin(tc suite.TestCtx) {
	for _, b := range p.cfg[tc.Suite].Backends {
		if b.Reset != "" {
			if _, err := p.run(tc, b, b.Reset, "reset"); err != nil {
				p.mark(tc, nil, true, false)
			}
		}
	}
}

func (p *Planner) end(tc suite.TestCtx) {
	for _, b := range p.cfg[tc.Suite].Backends {
		files, incomplete, err := p.dump(tc, b)
		if err != nil {
			p.mark(tc, nil, true, false)
			continue
		}
		p.mark(tc, files, false, incomplete)
	}
}

func (p *Planner) mark(tc suite.TestCtx, files []string, failed, incomplete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runs[tc.Run] == nil {
		p.runs[tc.Run] = map[string]*measured{}
	}
	m := p.runs[tc.Run][tc.Test]
	if m == nil {
		m = &measured{files: map[string]bool{}}
		p.runs[tc.Run][tc.Test] = m
	}
	m.failed = m.failed || failed
	m.incomplete = m.incomplete || incomplete
	for _, f := range files {
		m.files[f] = true
	}
}

func (p *Planner) dump(tc suite.TestCtx, b Backend) ([]string, bool, error) {
	file, err := p.run(tc, b, b.Dump, "dump")
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(file)
	if err != nil || len(data) == 0 {
		// The command printed its report instead of writing the file.
		data, err = os.ReadFile(file + ".log")
		if err != nil {
			return nil, false, err
		}
	}
	root := tc.Dirs[b.Repo]
	rel, incomplete, err := ParseDump(b.Format, data, root, func() []string {
		out, err := git(context.Background(), root, "ls-files", "-z")
		if err != nil {
			return nil
		}
		return strings.Split(out, "\x00")
	})
	if err != nil {
		return nil, false, err
	}
	for i := range rel {
		rel[i] = b.Repo + ":" + rel[i]
	}
	return rel, incomplete, nil
}

// run executes a coverage command for the test and returns the file its report goes to.
func (p *Planner) run(tc suite.TestCtx, b Backend, raw, what string) (string, error) {
	if tc.Expand == nil || tc.Out == "" || tc.Dirs[b.Repo] == "" {
		return "", fmt.Errorf("test %s is not on a shard", tc.Test)
	}
	cmd, err := tc.Expand(raw)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(tc.Out, "coverage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(dir, b.Stack+"-"+safe(tc.Test))
	file := base + ".dump"
	os.Remove(file)
	env := append(slices.Clone(tc.Env), "LOCALCI_OUTPUT_DIR="+tc.Out, "LOCALCI_TEST="+tc.Test, "LOCALCI_COVERAGE_FILE="+file)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := exec.Run(ctx, exec.Spec{Cmd: cmd, Dir: tc.Dirs[b.Repo], Env: env, LogPath: file + ".log", KillGrace: 2 * time.Second})
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("coverage %s of %s failed (exit %d)", what, b.Stack, res.ExitCode)
	}
	return file, nil
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

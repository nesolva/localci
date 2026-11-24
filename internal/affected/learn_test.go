package affected

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
	"gopkg.in/yaml.v3"
)

func gitRepo(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("x\n"), 0o644)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		if out, err := osexec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func testSuite() *suite.Suite {
	return &suite.Suite{Name: "e2e", Repos: []string{shop}, Test: suite.Test{Repo: shop}}
}

func newPlanner(t *testing.T) (*Planner, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Planner{Store: st, cfg: map[string]Config{}, runs: map[string]map[string]*measured{}}, st
}

func filesIn(t *testing.T, st *store.Store, test string) []string {
	t.Helper()
	m, err := st.CoverageMap(context.Background(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	return m[test].Files
}

// A fork's report is written by the code under test: an approved fork must not be able to empty a test's set
// and make later runs skip it. A writer's run teaches the map.
func TestForkRunLeavesTheMapAloneAndAWritersRunUpdatesIt(t *testing.T) {
	p, st := newPlanner(t)
	ctx := context.Background()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "public/pay.js")
	head := commitFile(t, dir, "public/cart.js")
	repos := map[string]suite.RepoState{shop: {Head: head, Dir: dir, Mirror: &exec.Mirror{Dir: dir}}}
	done := func(fork bool, reported ...string) {
		p.Done(ctx, suite.DoneInput{Run: "1", Suite: testSuite(), Fork: fork, Repos: repos,
			Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}}, Listed: []suite.TestRef{{ID: "t1"}},
			Frontend: map[string][]string{"t1": reported}})
	}

	done(false, "public/pay.js")
	if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":public/pay.js"}) {
		t.Fatalf("a writer's run left %v", got)
	}
	done(true, "public/cart.js")
	if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":public/pay.js"}) {
		t.Fatalf("a fork's run changed the map to %v", got)
	}
	done(false, "public/cart.js")
	if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":public/cart.js"}) {
		t.Fatalf("the next writer's run left %v", got)
	}
	// A default-branch run that cannot place one of the files leaves the test unknown, not half known.
	done(false, "public/cart.js", "/somewhere/not-tracked.js")
	if got := filesIn(t, st, "t1"); got != nil {
		t.Fatalf("a run with a hole in its report left %v: the test would skip changes in the file that was dropped", got)
	}
}

func TestOnlyPassedTestsWithUsableCoverageTeachTheMap(t *testing.T) {
	p, st := newPlanner(t)
	dir := gitRepo(t, "public/pay.js")
	p.Done(context.Background(), suite.DoneInput{Run: "1", Suite: testSuite(), Repos: map[string]suite.RepoState{shop: {Head: "abc", Dir: dir}},
		Passed: []string{"ok", "unusable"}, Ran: []suite.TestRef{{ID: "ok"}, {ID: "failed"}, {ID: "unusable"}, {ID: "silent"}},
		Frontend: map[string][]string{
			"ok":       {"public/pay.js"},
			"failed":   {"public/pay.js"},
			"unusable": {"dist/bundle.js"}, // names nothing the repo tracks: no basis to skip the test
		}})
	m, _ := st.CoverageMap(context.Background(), "e2e")
	if len(m) != 1 || m["ok"].Commits[shop] != "abc" {
		t.Fatalf("map = %+v, want only the passed test with usable coverage, with its commit", m)
	}
}

func TestBackendDumpsAreCollectedPerTestAndQualifiedWithTheServiceRepo(t *testing.T) {
	p, st := newPlanner(t)
	api := gitRepo(t, "src/orders.js", "src/pay.js")
	web := gitRepo(t, "public/pay.js")
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(`
repos: [acme/shop, acme/api]
test: {run: "true"}
stack:
  - {name: api, repo: acme/api, run: "true", ready: {log: x}}
coverage:
  - stack: api
    reset: 'echo reset > "$LOCALCI_OUTPUT_DIR/reset.mark"'
    dump: 'printf "src/orders.js\nsrc/pay.js\n" > "$LOCALCI_COVERAGE_FILE"'
    format: files
`), &node); err != nil {
		t.Fatal(err)
	}
	s, err := suite.Parse("e2e", node.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	p.cfg["e2e"] = cfg
	out := t.TempDir()
	tc := suite.TestCtx{Run: "1", Suite: "e2e", Test: "t1", Out: out, Dirs: map[string]string{"acme/api": api, shop: web},
		Expand: func(s string) (string, error) { return s, nil }}
	p.begin(tc)
	p.end(tc)
	if _, err := os.Stat(filepath.Join(out, "reset.mark")); err != nil {
		t.Fatalf("reset did not run in the shard's output dir: %v", err)
	}

	p.Done(context.Background(), suite.DoneInput{Run: "1", Suite: s, Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}},
		Repos:    map[string]suite.RepoState{shop: {Head: "w1", Dir: web}, "acme/api": {Head: "a1", Dir: api}},
		Frontend: map[string][]string{"t1": {"public/pay.js"}}})
	want := []string{"acme/api:src/orders.js", "acme/api:src/pay.js", shop + ":public/pay.js"}
	if got := filesIn(t, st, "t1"); !slices.Equal(got, want) {
		t.Fatalf("map = %v, want frontend and backend files together: %v", got, want)
	}
}

func TestACoverageDumpThatFailsKeepsTheTestRunning(t *testing.T) {
	p, st := newPlanner(t)
	api := gitRepo(t, "src/a.js")
	p.cfg["e2e"] = Config{Backends: []Backend{{Stack: "api", Repo: shop, Dump: "exit 3", Format: "files"}}}
	tc := suite.TestCtx{Run: "1", Suite: "e2e", Test: "t1", Out: t.TempDir(), Dirs: map[string]string{shop: api},
		Expand: func(s string) (string, error) { return s, nil }}
	p.end(tc)
	p.Done(context.Background(), suite.DoneInput{Run: "1", Suite: testSuite(), Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}},
		Repos: map[string]suite.RepoState{shop: {Head: "x", Dir: api}}})
	if m, _ := st.CoverageMap(context.Background(), "e2e"); len(m) != 0 {
		t.Fatalf("a partial measurement became %+v: it would shrink the test's set", m)
	}
}

func TestConfigRejectsWrongCoverageKeys(t *testing.T) {
	for name, body := range map[string]string{
		"unknown stack":  `coverage: [{stack: db, dump: x, format: files}]`,
		"unknown format": `coverage: [{stack: api, dump: x, format: gcov}]`,
		"no dump":        `coverage: [{stack: api, format: files}]`,
	} {
		var node yaml.Node
		yaml.Unmarshal([]byte("repos: [acme/shop]\ntest: {run: \"true\"}\nstack: [{name: api, run: \"true\", ready: {log: x}}]\n"+body), &node)
		s, err := suite.Parse("e2e", node.Content[0])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := ParseConfig(s); err == nil {
			t.Errorf("%s: want a config error", name)
		}
	}
}

// A test whose browser reported no files has no known set: storing it as an empty entry would make every later
// affected run skip it. It stays unknown, so it always runs.
func TestAnEmptyFrontendReportLeavesTheTestUnknown(t *testing.T) {
	p, st := newPlanner(t)
	dir := gitRepo(t, "public/pay.js")
	p.Done(context.Background(), suite.DoneInput{Run: "1", Suite: testSuite(), Repos: map[string]suite.RepoState{shop: {Head: "abc", Dir: dir}},
		Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}}, Frontend: map[string][]string{"t1": {}}})
	if m, _ := st.CoverageMap(context.Background(), "e2e"); len(m) != 0 {
		t.Fatalf("map = %+v, want no entry for a test that reported no files", m)
	}
}

// The map describes the default branch. A branch or working-tree run sees fewer or different files (a feature
// flag, a half-done change), so it may add files to a test's set but never take one away; only a default-branch
// run replaces the set.
func TestOnlyADefaultBranchRunReplacesATestsSetOtherRunsAddToIt(t *testing.T) {
	p, st := newPlanner(t)
	ctx := context.Background()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "public/pay.js")
	mainHead := commitFile(t, dir, "public/cart.js")
	gitIn(t, dir, "checkout", "-q", "-b", "feat")
	featHead := commitFile(t, dir, "public/promo.js")
	done := func(head string, reported ...string) {
		p.Done(ctx, suite.DoneInput{Run: "1", Suite: testSuite(), Repos: map[string]suite.RepoState{shop: {Head: head, Dir: dir, Mirror: &exec.Mirror{Dir: dir}}},
			Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}}, Frontend: map[string][]string{"t1": reported}})
	}
	// The mirror's own HEAD is the default branch.
	gitIn(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")

	done(mainHead, "public/pay.js", "public/cart.js")
	done(featHead, "public/pay.js", "public/promo.js") // the branch no longer reaches cart.js
	want := []string{shop + ":public/cart.js", shop + ":public/pay.js", shop + ":public/promo.js"}
	if got := filesIn(t, st, "t1"); !slices.Equal(got, want) {
		t.Fatalf("after a branch run the set is %v, want %v: the branch must not drop what main reaches", got, want)
	}
	if m, _ := st.CoverageMap(ctx, "e2e"); m["t1"].Commits[shop] != mainHead {
		t.Fatalf("a branch run moved the entry's commit to %s: the entry was still measured on main at %s", m["t1"].Commits[shop], mainHead)
	}
	done(mainHead, "public/cart.js")
	if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":public/cart.js"}) {
		t.Fatalf("a default-branch run left %v, want it to replace the set", got)
	}
}

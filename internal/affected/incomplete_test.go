package affected

import (
	"context"
	"slices"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/suite"
)

// A test whose coverage was only partly understood must keep running on every push: a change in the source that
// could not be attributed would otherwise select nothing and let a regression through.
func TestATestWithPartlyUnderstoodCoverageIsNeverSkipped(t *testing.T) {
	p, st := newPlanner(t)
	dir := gitRepo(t, "public/pay.js", "public/cart.js")
	done := func(in suite.DoneInput) {
		in.Run, in.Suite = "1", testSuite()
		in.Repos = map[string]suite.RepoState{shop: {Head: "abc", Dir: dir}}
		in.Passed, in.Ran = []string{"t1"}, []suite.TestRef{{ID: "t1"}}
		p.Done(context.Background(), in)
	}

	t.Run("the package says a source could not be attributed", func(t *testing.T) {
		done(suite.DoneInput{Frontend: map[string][]string{"t1": {"public/pay.js"}}, Incomplete: map[string]bool{"t1": true}})
		if m, _ := st.CoverageMap(context.Background(), "e2e"); len(m) != 0 {
			t.Fatalf("map = %+v: a test with a partly known set was stored as known", m)
		}
	})
	t.Run("a reported file the repository does not track", func(t *testing.T) {
		done(suite.DoneInput{Frontend: map[string][]string{"t1": {"public/pay.js", "assets/index-4f2a.js"}}})
		if m, _ := st.CoverageMap(context.Background(), "e2e"); len(m) != 0 {
			t.Fatalf("map = %+v: the file that was dropped would never select the test", m)
		}
	})
	t.Run("a fully understood report is still stored", func(t *testing.T) {
		done(suite.DoneInput{Frontend: map[string][]string{"t1": {"public/pay.js", "public/cart.js"}}})
		if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":public/cart.js", shop + ":public/pay.js"}) {
			t.Fatalf("files = %v", got)
		}
	})
}

func TestADumpNamingFilesTheRepoDoesNotHoldIsIncomplete(t *testing.T) {
	cases := []struct {
		name, format, data string
		incomplete         bool
	}{
		{"every file inside the repo", "files", "src/orders.js\nnode_modules/dep/index.js\n", false},
		{"a file outside the repo", "files", "src/orders.js\n/elsewhere/other.js\n", true},
		{"a path that climbs out of it", "files", "src/orders.js\n../outside.js\n", true},
		{"a lcov file outside the repo", "lcov", "SF:/elsewhere/a.js\nDA:1,3\nend_of_record\n", true},
		{"a lcov file that only loaded", "lcov", "SF:/elsewhere/a.js\nDA:1,0\nend_of_record\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, incomplete, err := ParseDump(tc.format, []byte(tc.data), "/work/api", nil)
			if err != nil || incomplete != tc.incomplete {
				t.Fatalf("incomplete = %v, %v; want %v", incomplete, err, tc.incomplete)
			}
		})
	}
	t.Run("a jacoco class no tracked file holds", func(t *testing.T) {
		_, incomplete, err := ParseDump("jacoco-xml", []byte(`<report><package name="com/acme"><sourcefile name="Gone.java"><counter type="LINE" missed="0" covered="3"/></sourcefile></package></report>`),
			"/work/api", func() []string { return []string{"src/main/java/com/acme/Other.java"} })
		if err != nil || !incomplete {
			t.Fatalf("incomplete = %v, %v", incomplete, err)
		}
	})
}

func TestABackendMeasurementWithAHoleIsNotStoredAndDropsAnOlderEntry(t *testing.T) {
	p, st := newPlanner(t)
	p.cfg["e2e"] = Config{Backends: []Backend{{Stack: "api", Repo: shop}}}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	head := commitFile(t, dir, "src/orders.js")
	done := func() {
		p.Done(context.Background(), suite.DoneInput{Run: "1", Suite: testSuite(),
			Repos:  map[string]suite.RepoState{shop: {Head: head, Dir: dir, Mirror: &exec.Mirror{Dir: dir}}},
			Passed: []string{"t1"}, Ran: []suite.TestRef{{ID: "t1"}}})
	}
	tc := suite.TestCtx{Run: "1", Test: "t1", Suite: "e2e"}

	p.mark(tc, []string{shop + ":src/orders.js"}, false, false)
	done()
	if got := filesIn(t, st, "t1"); !slices.Equal(got, []string{shop + ":src/orders.js"}) {
		t.Fatalf("a complete measurement left %v", got)
	}
	p.mark(tc, []string{shop + ":src/orders.js"}, false, true)
	done()
	if got := filesIn(t, st, "t1"); got != nil {
		t.Fatalf("a measurement with a hole left %v", got)
	}
}

package suite

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// An explicit filter that selects nothing is a failed run, whatever kind of filter it is: "nothing ran" must never
// read as "passed".
func TestRunFilterMatchesNothing(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	for _, c := range []struct {
		name   string
		filter Filter
		reason string
	}{
		{"file", Filter{Files: []string{"t/gone.spec"}}, "no tests matched t/gone.spec"},
		{"grep", Filter{Grep: "no such title"}, `no tests matched --grep "no such title"`},
		// The tests that failed in an earlier run may have been renamed since.
		{"failed", Filter{FailedOf: "999"}, "no tests matched --failed 999"},
	} {
		run := g.run(Request{Suite: "e2e", Heads: mainHead(), Tests: c.filter})
		if run.State != store.StateFailed || run.Reason != c.reason {
			t.Errorf("%s: run ended %s %q, want failed %q", c.name, run.State, run.Reason, c.reason)
		}
	}
}

// commitTree commits empty files, in folders, to the rig's repository.
func (g *rig) commitTree(paths ...string) {
	g.t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(g.src, p)), 0o755); err != nil {
			g.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(g.src, p), []byte(p+"\n"), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	git(g.t, g.src, "add", "-A")
	git(g.t, g.src, "commit", "-q", "-m", "tree")
}

// `run tests/` selects every spec under the directory the user typed, even though the framework lists files
// relative to its own test directory, and a directory that is not above the specs still selects nothing.
func TestFileFilterTestDir(t *testing.T) {
	// The list command names the specs relative to e2e/tests, as Playwright does with testDir: e2e/tests.
	yaml := `suites:
  e2e:
    repos: [acme/web]
    test:
      run: '"$FAKE_BIN" __fake run'
      list: |
        printf '{"test":"cart","file":"cart.spec.ts","title":"cart","line":1}\n{"test":"pay","file":"checkout/pay.spec.ts","title":"pay","line":1}\n{"test":"gen","file":"generated.spec.ts","title":"gen","line":1}\n'
    shards: 1
`
	g := newRig(t, yaml, nil, 4)
	g.commitTree("e2e/tests/cart.spec.ts", "e2e/tests/checkout/pay.spec.ts", "e2e/helpers/login.ts", "unit/tests/math.ts",
		"src/app.ts")

	for _, c := range []struct {
		filter string
		want   []string // nil: the run fails, nothing matched
	}{
		// generated.spec.ts is not tracked (a prepare step makes such specs): it is in the test directory all the same.
		{"e2e/tests/", []string{"cart", "gen", "pay"}},         // the test directory itself
		{"e2e", []string{"cart", "gen", "pay"}},                // a directory above it
		{".", []string{"cart", "gen", "pay"}},                  // the repository root
		{g.src + "/e2e/tests", []string{"cart", "gen", "pay"}}, // the same, typed as an absolute path in the checkout
		{"e2e/tests/checkout", []string{"pay"}},
		{"e2e/tests/cart.spec.ts", []string{"cart"}},
		{"e2e/tset/", nil},   // a typo
		{"e2e/helpers", nil}, // a directory with no specs
		{"unit/", nil},
		{"src", nil},
	} {
		// The run tests the working tree of a checkout, as one started from the command line does.
		heads := mainHead()
		heads[0].Dir = g.src
		run := g.run(Request{Suite: "e2e", Heads: heads, Tests: Filter{Files: []string{c.filter}}})
		tests, err := g.st.RunTests(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, x := range tests {
			got = append(got, x.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("run %s selected %v, want %v (run %s: %s)", c.filter, got, c.want, run.State, run.Reason)
		}
		if c.want == nil && (run.State != store.StateFailed || !strings.HasPrefix(run.Reason, "no tests matched")) {
			t.Errorf("run %s ended %s %q, want failed, no tests matched", c.filter, run.State, run.Reason)
		}
	}
}

// A test command that hangs is stopped by the suite's timeout, and the run says so.
func TestSuiteTimeoutExit4(t *testing.T) {
	yaml := `suites:
  e2e:
    repos: [acme/web]
    test:
      run: exec sleep 300
      list: |
        printf '{"test":"a","file":"a.spec.ts","title":"a","line":1}\n'
    shards: 1
    timeout: 2s
`
	g := newRig(t, yaml, nil, 4)
	started := time.Now()
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	if run.State != store.StateFailed || run.Reason != "timed out after 2s" {
		t.Fatalf("run ended %s %q, want failed, timed out after 2s", run.State, run.Reason)
	}
	// The code an agent's exit status comes from is a field of the run, so nobody has to read the reason.
	if b, _ := json.Marshal(run); !strings.Contains(string(b), `"outcome":"timeout"`) {
		t.Errorf("the run carries no timeout outcome: %s", b)
	}
	if took := time.Since(started); took > time.Minute {
		t.Errorf("the run took %s to end", took)
	}
}

// A suite that sets no timeout cannot block the queue for ever: it gets 60 minutes. `timeout: 0` is how to turn
// that off.
func TestSuiteTimeoutDefault(t *testing.T) {
	for line, want := range map[string]time.Duration{
		"":                60 * time.Minute,
		"    timeout:":    60 * time.Minute,
		"    timeout: 0":  0,
		"    timeout: 0s": 0,
		"    timeout: 5m": 5 * time.Minute,
	} {
		suites, err := ParseYAML([]byte("suites:\n  e2e:\n    repos: [acme/web]\n    test: {run: \"true\"}\n" + line + "\n"))
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		if got := suites[0].Timeout; got != want {
			t.Errorf("%q: timeout %s, want %s", line, got, want)
		}
	}
}

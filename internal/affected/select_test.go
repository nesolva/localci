package affected

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

const shop = "acme/shop"

// sample models examples/acme-shop's spec files and the app files each one reached.
func sample() Input {
	spec := func(name string) Test { return Test{ID: "t-" + name, File: "tests/" + name + ".spec.ts", Title: name} }
	files := func(f ...string) []string {
		out := make([]string, len(f))
		for i, x := range f {
			out[i] = shop + ":" + x
		}
		return out
	}
	common := []string{"public/app.js", "public/cart.js"}
	return Input{
		TestRepo: shop,
		Tests:    []Test{spec("auth"), spec("cart"), spec("catalog"), spec("checkout"), spec("shops")},
		Map: map[string][]string{
			"t-auth":     files(append(common, "public/login.js")...),
			"t-cart":     files(common...),
			"t-catalog":  files("public/app.js", "public/catalog.js"),
			"t-checkout": files(append(common, "public/checkout.js", "public/pay.js")...),
			"t-shops":    files("public/app.js", "public/shops.js"),
		},
		AgeCommits: 3,
	}
}

func ids(s Selection) string { return strings.Join(s.IDs(), ",") }

func TestSampleFileOnlyTheCheckoutPageUsesSelectsOnlyCheckoutTests(t *testing.T) {
	in := sample()
	in.Changed = []string{shop + ":public/pay.js"}
	sel := Select(in)
	if sel.All || ids(sel) != "t-checkout" {
		t.Fatalf("selected %q (all=%v, %s), want only the checkout test", ids(sel), sel.All, sel.Reason)
	}
	in.Changed = []string{shop + ":public/cart.js"}
	if got := ids(Select(in)); got != "t-auth,t-cart,t-checkout" {
		t.Fatalf("a file three tests reach selected %q", got)
	}
}

func TestSelectRunsEverythingWhenTheChoiceIsNotSafe(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Input)
		changed string
		reason  string
	}{
		{"no map yet", func(in *Input) { in.Map = nil }, "public/pay.js", "no coverage map"},
		{"map too old", func(in *Input) { in.AgeCommits = MaxAge + 1 }, "public/pay.js", "200 commits"},
		{"age not computable", func(in *Input) { in.AgeCommits = -1 }, "public/pay.js", "could not be told"},
		{"lockfile", nil, "package-lock.json", "always_all"},
		{"nested lockfile", nil, "web/pnpm-lock.yaml", "always_all"},
		{"migration", nil, "db/migrations/001_orders.sql", "always_all"},
		{"tool config", nil, "playwright.config.ts", "always_all"},
		{"prepare key", func(in *Input) { in.AlwaysAll = []string{shop + ":requirements/*.txt"} }, "requirements/base.txt", "always_all"},
		{"source file no test reaches", nil, "public/new-page.js", "no test's coverage"},
		{"deleted source file nobody reached", nil, "public/old.js", "no test's coverage"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := sample()
			if c.mutate != nil {
				c.mutate(&in)
			}
			in.Changed = []string{shop + ":" + c.changed}
			sel := Select(in)
			if !sel.All || !strings.Contains(sel.Reason, c.reason) {
				t.Fatalf("got all=%v reason=%q, want all tests because %q", sel.All, sel.Reason, c.reason)
			}
		})
	}
}

func TestSelectLeavesWhatCannotChangeABrowserAndRunsTestChangesAlone(t *testing.T) {
	in := sample()
	in.Tests = append(in.Tests, Test{ID: "t-new", File: "tests/new.spec.ts", Title: "new"})
	in.Changed = []string{shop + ":README.md", shop + ":docs/setup.md", shop + ":tests/cart.spec.ts"}
	sel := Select(in)
	// The test with no map entry is new: it always runs. Docs select nothing. A changed spec runs itself.
	if sel.All || ids(sel) != "t-cart,t-new" {
		t.Fatalf("selected %q (all=%v %s)", ids(sel), sel.All, sel.Reason)
	}
	in.Changed = []string{shop + ":tests/fixtures.ts"}
	if sel := Select(in); !sel.All {
		t.Fatalf("a helper next to the tests may be used by any test, got %q", ids(sel))
	}
	in.Changed = nil
	if sel := Select(in); sel.All || ids(sel) != "t-new" {
		t.Fatalf("nothing changed selected %q, want only the test nobody measured", ids(sel))
	}
}

// No false negatives: whatever the maps and the diffs, a test whose set holds a changed file runs, unless
// everything runs.
func TestSelectNeverSkipsATestThatReachesAChangedFile(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	pool := make([]string, 30)
	for i := range pool {
		pool[i] = fmt.Sprintf("%s:src/f%02d.js", shop, i)
	}
	pick := func(n int) []string {
		var out []string
		for _, i := range rng.Perm(len(pool))[:n] {
			out = append(out, pool[i])
		}
		return out
	}
	for round := 0; round < 500; round++ {
		in := Input{TestRepo: shop, AgeCommits: rng.Intn(MaxAge), Map: map[string][]string{}}
		for i := 0; i < 1+rng.Intn(12); i++ {
			id := fmt.Sprintf("t%d", i)
			in.Tests = append(in.Tests, Test{ID: id, File: fmt.Sprintf("tests/s%d.spec.ts", i)})
			in.Map[id] = pick(rng.Intn(8))
		}
		in.Changed = pick(1 + rng.Intn(4))
		sel := Select(in)
		if sel.All {
			continue
		}
		got := ids(sel)
		for _, tc := range in.Tests {
			for _, f := range in.Changed {
				if slices.Contains(in.Map[tc.ID], f) && !slices.Contains(sel.IDs(), tc.ID) {
					t.Fatalf("round %d: %s reaches %s but was skipped (selected %s)", round, tc.ID, f, got)
				}
			}
		}
	}
}

// A text or docs-folder file can be a dependency list or a fixture a test reads: only prose formats are skipped.
func TestSelectRunsEverythingWhenANonProseFileOutsideAnyCoverageChanges(t *testing.T) {
	for _, f := range []string{"requirements.txt", "content/page.mdx", "docs/examples/app.js"} {
		in := sample()
		in.Changed = []string{shop + ":" + f}
		if sel := Select(in); !sel.All || !strings.Contains(sel.Reason, "no test's coverage") {
			t.Errorf("%s: got all=%v reason=%q, want every test", f, sel.All, sel.Reason)
		}
	}
}

// A helper in the test repository that no test's coverage names (a node-side import: browsers never load it) may
// be used by any spec, whichever directory the helper or the spec sits in.
func TestSelectRunsEverythingWhenAHelperInTheTestRepoIsUsedAcrossDirectories(t *testing.T) {
	in := sample()
	in.Tests = []Test{
		{ID: "t-a", File: "tests/a/login.spec.ts"},
		{ID: "t-b", File: "tests/b/cart.spec.ts"},
	}
	in.Map = map[string][]string{"t-a": {shop + ":public/app.js"}, "t-b": {shop + ":public/app.js"}}
	in.Changed = []string{shop + ":tests/a/helpers.ts"}
	if sel := Select(in); !sel.All || !strings.Contains(sel.Reason, "tests/a/helpers.ts") {
		t.Fatalf("selected %q (all=%v %s), want every test: tests/b may import the helper", ids(sel), sel.All, sel.Reason)
	}
}

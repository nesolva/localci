// Package affected chooses the e2e tests a diff can affect from per-test coverage, and learns that coverage from
// the runs: the frontend through the Playwright package, the backend through dumps the suite configures.
package affected

import (
	"path"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// MaxAge is how many commits on the base a measurement may be behind before every test runs again.
const MaxAge = 200

// DefaultAlwaysAll are the changes whose reach no coverage can tell: dependency locks, migrations and tool
// configuration. A pattern without a slash matches the file name in any directory.
var DefaultAlwaysAll = []string{
	"package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb", "go.sum", "Cargo.lock",
	"poetry.lock", "uv.lock", "Pipfile.lock", "Gemfile.lock", "composer.lock", "gradle.lockfile",
	"**/migrations/**", "**/migrate/**", "*.config.*",
}

// Test is a listed test; File is relative to the test repository.
type Test struct {
	ID, File, Title string
}

// Input is everything Select needs; it does no I/O. Files are repo-qualified: "owner/name:path/in/repo".
type Input struct {
	Changed  []string
	Tests    []Test
	TestRepo string
	Map      map[string][]string // test id → files it touched
	// AgeCommits is how far behind the base the oldest measurement is; -1 when it cannot be computed.
	AgeCommits int
	AlwaysAll  []string // added to DefaultAlwaysAll
}

// Item is one selected test and the changed files that selected it.
type Item struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Because []string `json:"because"`
}

// Selection is the outcome: every test (All) with the reason, or the Items that run.
type Selection struct {
	All     bool     `json:"all"`
	Reason  string   `json:"reason,omitempty"`
	Changed []string `json:"changed"`
	Items   []Item   `json:"items,omitempty"`
	Total   int      `json:"total"`
}

// IDs returns the selected test ids, in listing order.
func (s Selection) IDs() []string {
	out := make([]string, len(s.Items))
	for i, it := range s.Items {
		out[i] = it.ID
	}
	return out
}

func all(in Input, reason string) Selection {
	return Selection{All: true, Reason: reason, Changed: in.Changed, Total: len(in.Tests)}
}

// Select picks the tests to run. When the choice is not safe it says so: all of them, with the reason.
func Select(in Input) Selection {
	switch {
	case len(in.Map) == 0:
		return all(in, "no coverage map yet")
	case in.AgeCommits < 0:
		return all(in, "the age of the coverage map could not be told")
	case in.AgeCommits > MaxAge:
		return all(in, "the coverage map is more than 200 commits behind the base")
	}
	patterns := append(slices.Clone(DefaultAlwaysAll), in.AlwaysAll...)
	for _, f := range in.Changed {
		for _, p := range patterns {
			if matches(p, f) {
				return all(in, f+" matches always_all ("+p+")")
			}
		}
	}

	qual := func(t Test) string { return in.TestRepo + ":" + t.File }
	testFile := map[string]bool{}
	for _, t := range in.Tests {
		testFile[qual(t)] = true
	}
	covers := map[string]map[string]bool{} // file → tests whose set holds it
	for _, t := range in.Tests {
		for _, f := range in.Map[t.ID] {
			if covers[f] == nil {
				covers[f] = map[string]bool{}
			}
			covers[f][t.ID] = true
		}
	}

	because := map[string][]string{}
	add := func(id, f string) {
		if !slices.Contains(because[id], f) {
			because[id] = append(because[id], f)
		}
	}
	for _, t := range in.Tests {
		if _, ok := in.Map[t.ID]; !ok {
			add(t.ID, "no coverage yet")
		}
	}
	for _, f := range in.Changed {
		hit := false
		for _, t := range in.Tests {
			if qual(t) == f || covers[f][t.ID] {
				add(t.ID, f)
				hit = true
			}
		}
		if hit || testFile[f] || isDoc(f) {
			continue
		}
		// Nothing reached it, in the test repository or elsewhere: a helper any spec imports, or a file added
		// since the map was measured.
		return all(in, f+" is in no test's coverage")
	}

	sel := Selection{Changed: in.Changed, Total: len(in.Tests)}
	for _, t := range in.Tests {
		if b := because[t.ID]; len(b) > 0 {
			sel.Items = append(sel.Items, Item{ID: t.ID, Name: strings.TrimSpace(t.File + " " + t.Title), Because: b})
		}
	}
	return sel
}

func matches(pattern, file string) bool {
	repo, p, scoped := strings.Cut(pattern, ":")
	if !scoped {
		p = pattern
	}
	fr, fp, _ := strings.Cut(file, ":")
	if scoped && fr != repo {
		return false
	}
	if !strings.Contains(p, "/") {
		p = "**/" + p
	}
	ok, _ := doublestar.Match(p, fp)
	return ok
}

// isDoc says a changed file cannot change what a browser does.
func isDoc(file string) bool {
	_, p, _ := strings.Cut(file, ":")
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".rst":
		return true
	}
	return strings.HasPrefix(path.Base(p), "LICENSE")
}

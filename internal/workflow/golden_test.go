package workflow

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden plans")

// fixtureEvent reads <name>.event.json, a pull request touching one source file
// on main when absent.
func fixtureEvent(t *testing.T, base string) Event {
	t.Helper()
	ev := Event{Name: "pull_request", Repo: "acme/app", Branch: "main", Files: []string{"src/app.go"}}
	b, err := os.ReadFile(base + ".event.json")
	if os.IsNotExist(err) {
		return ev
	}
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Name   string   `json:"name"`
		Branch string   `json:"branch"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	ev.Name, ev.Branch, ev.Files = in.Name, in.Branch, in.Files
	return ev
}

func TestGolden(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/workflows/*.yml")
	if err != nil || len(fixtures) < 12 {
		t.Fatalf("want at least 12 fixtures, got %d (%v)", len(fixtures), err)
	}
	for _, f := range fixtures {
		base := strings.TrimSuffix(f, ".yml")
		t.Run(filepath.Base(base), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			res := PlanWorkflows("0123abc", fixtureEvent(t, base), map[string][]byte{
				".github/workflows/" + filepath.Base(f): data,
			})
			got, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			golden := base + ".golden.json"
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("plan differs from %s (run with -update after checking the diff)\n--- got ---\n%s", golden, got)
			}
		})
	}
}

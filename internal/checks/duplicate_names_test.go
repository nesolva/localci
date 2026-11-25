package checks

import (
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/workflow"
)

const duplicateCells = `name: CI
on: [pull_request]
jobs:
  test:
    name: test ${{ matrix.os }}
    runs-on: ubuntu-latest
    strategy:
      matrix:
        os: [linux]
        node: [18, 20]
    steps:
      - run: echo ran >> "$RUNLOG"; test "${{ matrix.node }}" != 18
  lint:
    runs-on: ubuntu-latest
    steps:
      - run: echo linted
`

// Two cells that share a name must not share a status: GitHub keeps one status per context, so the green
// one would hide the red one.
func TestDuplicateCellNames(t *testing.T) {
	fx := newFixture(t, duplicateCells)
	head := fx.pull("1", map[string]string{"a.txt": "x\n"})
	sub := fx.submit(fx.request(1, head))

	final := func(context string) string {
		got := fx.forge.of(context)
		if len(got) == 0 {
			return ""
		}
		return string(got[len(got)-1].State)
	}
	if got := final("CI/test linux (linux, 18)"); got != "failure" {
		t.Errorf("node 18 cell ends %q, want failure", got)
	}
	if got := final("CI/test linux (linux, 20)"); got != "success" {
		t.Errorf("node 20 cell ends %q, want success", got)
	}
	if got := final("CI/test linux"); got != "" {
		t.Errorf("the shared context was posted (%q): the cells would overwrite each other", got)
	}
	if got := final("CI/lint"); got != "success" {
		t.Errorf("a unique name keeps its context, got %q", got)
	}
	states := map[string]string{}
	for _, s := range sub.Runs {
		states[s.Context] = fx.run(s.RunID).State
	}
	if states["localci/CI/test linux (linux, 18)"] != store.StateFailed || states["localci/CI/test linux (linux, 20)"] != store.StatePassed {
		t.Errorf("run states = %v", states)
	}
}

func TestDuplicateCellNamesStillCollidingAreNumbered(t *testing.T) {
	check := func(name, cell string) workflow.Check { return workflow.Check{Workflow: "ci.yml", Name: name, Cell: cell} }
	got := UniqueNames([]workflow.Check{
		check("test", " (linux, 18)"), check("test", " (linux, 18)"), check("test", " (linux, 18)"), check("solo", ""),
	})
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if want := "test (linux, 18),test (linux, 18 #2),test (linux, 18 #3),solo"; strings.Join(names, ",") != want {
		t.Errorf("names = %v, want %s", names, want)
	}
}

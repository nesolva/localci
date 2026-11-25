package workflow

import "testing"

// A name that reads the matrix is left as written and keeps GitHub's cell suffix in Cell, for a caller that finds
// two checks under one name; any other name already carries it.
func TestDuplicateCellNamesCarryTheirCell(t *testing.T) {
	yml := `on: pull_request
jobs:
  a:
    name: test ${{ matrix.os }}
    runs-on: x
    strategy: {matrix: {os: [linux], node: [18]}}
    steps: [{run: echo a}]
  b:
    runs-on: x
    strategy: {matrix: {os: [linux], node: [18]}}
    steps: [{run: echo b}]
`
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
	got := map[string][2]string{}
	for _, c := range res.Checks {
		got[c.Job] = [2]string{c.Name, c.Cell}
	}
	if want := ([2]string{"test linux", " (linux, 18)"}); got["a"] != want {
		t.Errorf("a = %q, want %q", got["a"], want)
	}
	if want := ([2]string{"b (linux, 18)", ""}); got["b"] != want {
		t.Errorf("b = %q, want %q", got["b"], want)
	}
}

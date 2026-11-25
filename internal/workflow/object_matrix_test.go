package workflow

import (
	"strings"
	"testing"
)

const objectMatrix = `on: pull_request
jobs:
  build:
    name: ${{ matrix.config.name }}
    runs-on: x
    strategy:
      matrix:
        config:
          - {name: Linux, os: linux, node: 18, extra: {flag: on}}
          - {name: Windows, os: windows, node: 20}
    env:
      NODE: ${{ matrix.config.node }}
    steps:
      - if: matrix.config.os == 'linux'
        run: echo only-linux ${{ matrix.config.extra.flag }}
      - if: ${{ matrix.config.os != 'linux' }}
        run: echo not-linux
      - uses: actions/setup-node@v4
        with:
          node-version: ${{ matrix.config.node }}
      - run: echo missing=${{ matrix.config.nothing }}.${{ matrix.nothing.at.all }}
`

// matrix.config.name resolves through the object values of include: and of an object axis, with GitHub's rules:
// a missing key is null.
func TestObjectMatrix(t *testing.T) {
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(objectMatrix)})
	byName := map[string]Check{}
	for _, c := range res.Checks {
		byName[c.Name] = c
		if c.Unsupported != "" {
			t.Errorf("%s: unsupported: %s", c.Name, c.Unsupported)
		}
	}
	linux, win := byName["Linux"], byName["Windows"]
	if linux.Name == "" || win.Name == "" {
		t.Fatalf("names = %v, want Linux and Windows", len(byName))
	}
	runs := func(c Check) string {
		var out []string
		for _, s := range c.Steps {
			if s.Run != "" {
				out = append(out, s.Run)
			}
		}
		return strings.Join(out, "|")
	}
	if got := runs(linux); !strings.HasPrefix(got, "echo only-linux on|") || strings.Contains(got, "not-linux") {
		t.Errorf("linux steps = %q: the if: on matrix.config.os kept the wrong ones", got)
	}
	if got := runs(win); strings.Contains(got, "only-linux") || !strings.HasPrefix(got, "echo not-linux") {
		t.Errorf("windows steps = %q: the if: on matrix.config.os kept the wrong ones", got)
	}
	if linux.Env["NODE"] != "18" || win.Env["NODE"] != "20" {
		t.Errorf("env NODE = %q and %q, want 18 and 20", linux.Env["NODE"], win.Env["NODE"])
	}
	if got := win.Toolchain["node"]; got != "20" {
		t.Errorf("node toolchain (with: node-version) = %q, want 20", got)
	}
	if got := runs(win); !strings.HasSuffix(got, "echo missing=.") {
		t.Errorf("missing keys print %q, want empty text", got)
	}
}

func TestObjectMatrixInclude(t *testing.T) {
	yml := `on: pull_request
jobs:
  build:
    name: ${{ matrix.config.name }}
    runs-on: x
    strategy:
      matrix:
        include:
          - config: {name: Linux, os: linux}
          - config: {name: Mac, os: mac}
    steps:
      - if: matrix.config.os == 'mac'
        run: echo mac-only
`
	res := PlanWorkflows("0123abc", Event{Name: "pull_request", Repo: "a/b", Branch: "main"},
		map[string][]byte{".github/workflows/ci.yml": []byte(yml)})
	var got []string
	for _, c := range res.Checks {
		got = append(got, c.Name+":"+strings.Repeat("s", len(c.Steps)))
	}
	if strings.Join(got, ",") != "Linux:,Mac:s" {
		t.Errorf("cells = %v, want Linux with no step and Mac with one", got)
	}
}

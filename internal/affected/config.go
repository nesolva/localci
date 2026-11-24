package affected

import (
	"fmt"
	"slices"

	"github.com/nesolva/localci/internal/suite"
)

// Backend is how one stack service reports which of its files a test ran:
//
//	coverage:
//	  - stack: api
//	    reset: curl -s -X POST localhost:{{port "api"}}/__coverage/reset
//	    dump: curl -s localhost:{{port "api"}}/__coverage > "$LOCALCI_COVERAGE_FILE"
//	    format: lcov
//
// reset runs before every test and dump after it (the suite's placeholders work in both); the dump leaves its
// report in $LOCALCI_COVERAGE_FILE, or prints it. Shards run one test at a time, so a dump is one test's.
type Backend struct {
	Stack  string `yaml:"stack"`
	Reset  string `yaml:"reset"`
	Dump   string `yaml:"dump"`
	Format string `yaml:"format"`
	Repo   string `yaml:"-"` // the repository the stack service runs from
}

// Config is the suite's keys this package owns.
type Config struct {
	Backends []Backend
	// AlwaysAll are extra patterns, "glob" or "repo:glob", whose change runs every test.
	AlwaysAll []string
}

// ParseConfig reads coverage and always_all from the suite's own mapping.
func ParseConfig(s *suite.Suite) (Config, error) {
	var raw struct {
		Coverage  []Backend `yaml:"coverage"`
		AlwaysAll []string  `yaml:"always_all"`
	}
	if s.Raw != nil {
		if err := s.Raw.Decode(&raw); err != nil {
			return Config{}, fmt.Errorf("suites.%s: %v", s.Name, err)
		}
	}
	c := Config{AlwaysAll: raw.AlwaysAll}
	for i, b := range raw.Coverage {
		fail := func(f string, a ...any) (Config, error) {
			return Config{}, fmt.Errorf("suites.%s.coverage[%d]: %s", s.Name, i, fmt.Sprintf(f, a...))
		}
		idx := slices.IndexFunc(s.Stack, func(sv suite.Service) bool { return sv.Name == b.Stack })
		switch {
		case idx < 0:
			return fail("stack %q is not a service of the suite's stack", b.Stack)
		case b.Dump == "":
			return fail("dump: a command is required")
		case !slices.Contains(Formats, b.Format):
			return fail("format: %q is not known (use %s)", b.Format, joinFormats())
		}
		b.Repo = s.Stack[idx].Repo
		c.Backends = append(c.Backends, b)
	}
	return c, nil
}

func joinFormats() string {
	out := ""
	for i, f := range Formats {
		if i > 0 {
			out += ", "
		}
		out += f
	}
	return out
}

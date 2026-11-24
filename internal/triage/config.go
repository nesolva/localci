package triage

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config is `suites.<s>.triage`.
type Config struct {
	// Reruns is how many extra times a test that still fails after the runner's own reruns alone is run again to
	// rule out flakiness; 0 adds none.
	Reruns int `yaml:"reruns"`
	// Propose is what a test bug's fix may become: "patch" (default, a file under the run's log directory and
	// `localci triage --apply`) or "pr" (`localci triage --pr` may open a pull request with `gh`).
	Propose string `yaml:"propose"`
}

// MaxReruns bounds Config.Reruns: more only burns the machine.
const MaxReruns = 5

// ParseConfig reads the `triage` key of a suite's mapping node (nil when the suite has none).
func ParseConfig(suite *yaml.Node) (Config, error) {
	c := Config{Propose: "patch"}
	if suite == nil || suite.Kind != yaml.MappingNode {
		return c, nil
	}
	for i := 0; i+1 < len(suite.Content); i += 2 {
		if suite.Content[i].Value != "triage" {
			continue
		}
		if err := suite.Content[i+1].Decode(&c); err != nil {
			return Config{Propose: "patch"}, fmt.Errorf("triage: %w", err)
		}
	}
	if c.Propose == "" {
		c.Propose = "patch"
	}
	if c.Propose != "patch" && c.Propose != "pr" {
		return Config{Propose: "patch"}, fmt.Errorf("triage.propose is %q: use patch or pr", c.Propose)
	}
	if c.Reruns < 0 || c.Reruns > MaxReruns {
		return Config{Propose: "patch"}, fmt.Errorf("triage.reruns is %d: use 0 to %d", c.Reruns, MaxReruns)
	}
	return c, nil
}

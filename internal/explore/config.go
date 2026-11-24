package explore

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultCommand is the agent command when the config names none. Print mode denies Bash unless it is allowed, and
// without Bash the agent can neither drive the app nor write its findings. --max-turns bounds the cost together with
// agent.minutes.
const DefaultCommand = `claude -p --output-format stream-json --verbose --allowedTools "Bash,Read,Write" --max-turns 60`

// Limits on what the config may ask for.
const (
	DefaultMinutes = 30
	MaxMinutes     = 240
	MaxGoals       = 20
	MaxGoalLen     = 300
)

// Config is the `explore` section of the configuration.
type Config struct {
	// Schedule is a cron expression of five fields, in the machine's local time zone.
	Schedule string `yaml:"schedule"`
	// Suite is the suite whose stack is explored.
	Suite string `yaml:"suite"`
	// Goals are what the agent tries, in the team's words.
	Goals []string `yaml:"goals"`
	// BaseURL is where the app listens inside the shard, a template over the shard's ports: http://127.0.0.1:{{port "web"}}.
	BaseURL string `yaml:"base_url"`
	Agent   Agent  `yaml:"agent"`
}

// Agent is the command that explores.
type Agent struct {
	// Command is run through bash in the test repository's workspace with the brief on its standard input.
	Command string `yaml:"command"`
	// Minutes is how long it may run before it is killed; what it wrote is kept.
	Minutes int `yaml:"minutes"`
	// Env names the variables of localci's own environment that are passed to the agent when they are set. The agent
	// gets a minimal environment otherwise; use this for what a provider needs (AWS_REGION for Bedrock).
	Env []string `yaml:"env"`
}

// envName is what a variable name in agent.env looks like: no values, no wildcards.
var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Parse reads the section. It returns nil when there is none (exploration is off).
func Parse(section map[string]any) (*Config, error) {
	if len(section) == 0 {
		return nil, nil
	}
	b, err := yaml.Marshal(section)
	if err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("explore: %w", err)
	}
	if c.Agent.Command == "" {
		c.Agent.Command = DefaultCommand
	}
	if c.Agent.Minutes == 0 {
		c.Agent.Minutes = DefaultMinutes
	}
	if _, err := ParseCron(c.Schedule); err != nil {
		return nil, fmt.Errorf("explore.schedule: %w", err)
	}
	switch {
	case c.Suite == "":
		return nil, fmt.Errorf("explore.suite: name the suite whose stack the agent explores")
	case c.BaseURL == "":
		return nil, fmt.Errorf(`explore.base_url: say where the app listens, e.g. http://127.0.0.1:{{port "web"}}`)
	case len(c.Goals) == 0:
		return nil, fmt.Errorf("explore.goals: write at least one goal")
	case len(c.Goals) > MaxGoals:
		return nil, fmt.Errorf("explore.goals: %d goals, at most %d", len(c.Goals), MaxGoals)
	case c.Agent.Minutes < 1 || c.Agent.Minutes > MaxMinutes:
		return nil, fmt.Errorf("explore.agent.minutes is %d: use 1 to %d", c.Agent.Minutes, MaxMinutes)
	}
	for i, n := range c.Agent.Env {
		if !envName.MatchString(n) {
			return nil, fmt.Errorf("explore.agent.env[%d]: %q is not a variable name (capital letters, digits and _, no values or wildcards)", i, n)
		}
	}
	for i, g := range c.Goals {
		if strings.TrimSpace(g) == "" || len([]rune(g)) > MaxGoalLen {
			return nil, fmt.Errorf("explore.goals[%d]: a goal is 1 to %d characters", i, MaxGoalLen)
		}
	}
	return &c, nil
}

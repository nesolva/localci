package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/nesolva/localci/internal/explore"
)

func init() {
	register("config", "show the effective configuration and where each value comes from", false, cmdConfig)
}

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	var asJSON *bool
	eff, _, code := load("config", args, stderr, func(fs *flag.FlagSet) {
		asJSON = fs.Bool("json", false, "print JSON: every value with its source")
	})
	if eff == nil {
		return code
	}
	if *asJSON {
		b, err := eff.JSON()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	}
	if eff.FileFound {
		fmt.Fprintf(stdout, "# config file: %s\n", eff.File)
	} else {
		fmt.Fprintf(stdout, "# config file: %s (not found, using defaults)\n", eff.File)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, l := range eff.Lines() {
		fmt.Fprintf(tw, "%s\t%s\t(%s)\n", l.Key, l.Value, l.Source)
		if l.Key == "explore" {
			exploreLines(tw, eff.Config.Explore)
		}
	}
	tw.Flush()
	if err := eff.Validate(); err != nil {
		fmt.Fprintln(stderr, "note:", err)
	}
	return 0
}

// exploreLines shows what the explore section resolves to once its defaults apply: the agent command is the one
// thing a reader of `explore` cannot see in the file when they left it out.
func exploreLines(w io.Writer, section map[string]any) {
	c, err := explore.Parse(section)
	if err != nil || c == nil {
		return
	}
	agent, _ := section["agent"].(map[string]any)
	source := func(key string) string {
		if _, ok := agent[key]; ok {
			return "file"
		}
		return "default"
	}
	fmt.Fprintf(w, "explore.agent.command\t%s\t(%s)\n", c.Agent.Command, source("command"))
	fmt.Fprintf(w, "explore.agent.minutes\t%d\t(%s)\n", c.Agent.Minutes, source("minutes"))
	env := "(none)"
	if len(c.Agent.Env) > 0 {
		env = strings.Join(c.Agent.Env, ", ")
	}
	fmt.Fprintf(w, "explore.agent.env\t%s\t(%s)\n", env, source("env"))
}

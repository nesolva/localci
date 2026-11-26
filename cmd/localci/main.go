// Command localci runs a team's e2e suites and pull-request checks on its own machines, and answers agents in a
// few tokens.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// Exit codes: what an agent branches on.
const (
	exitOK      = 0 // passed
	exitFailed  = 1 // tests failed
	exitUsage   = 2 // wrong usage or configuration, a filter that matched no test included
	exitInfra   = 3 // infrastructure: no server, a run that broke before its tests, a cancelled run
	exitTimeout = 4 // a run passed its suite's timeout, or `wait` gave up
)

type command struct {
	name, summary string
	agent         bool // listed with the agent commands
	run           func(args []string, stdout, stderr io.Writer) int
}

var commands []command

// register adds a command; each command's file calls it from init().
func register(name, summary string, agent bool, run func(args []string, stdout, stderr io.Writer) int) {
	commands = append(commands, command{name, summary, agent, run})
}

func usage() string {
	var b strings.Builder
	b.WriteString("localci runs e2e suites and pull-request checks on your own machines, for people and agents.\n\n")
	b.WriteString("Usage: localci <command> [flags]\n")
	for _, agent := range []bool{true, false} {
		if agent {
			b.WriteString("\nFor agents (one line per item, --json for NDJSON, exit 0 passed, 1 tests failed, 2 usage, 3 infrastructure, 4 timeout):\n")
		} else {
			b.WriteString("\nFor people:\n")
		}
		cmds := slices.Clone(commands)
		slices.SortFunc(cmds, func(a, b command) int { return strings.Compare(a.name, b.name) })
		for _, c := range cmds {
			if c.agent == agent {
				fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.summary)
			}
		}
	}
	b.WriteString("\nRun \"localci <command> -h\" for the flags of a command.\n")
	return b.String()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usage())
		return exitOK
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "localci: unknown command %q\n\n%s", args[0], usage())
	return exitUsage
}

// load parses the shared config flags and loads the effective configuration.
func load(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (*config.Effective, *flag.FlagSet, int) {
	fs := flag.NewFlagSet("localci "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "config file (default ~/.config/localci/config.yaml)")
	if extra != nil {
		extra(fs)
	}
	fv := config.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil, fs, exitOK
		}
		return nil, fs, exitUsage
	}
	eff, err := config.Load(config.Options{
		Path:    *path,
		Flags:   fv.Values(),
		Machine: capacity.Detect(context.Background()),
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return nil, fs, exitUsage
	}
	return eff, fs, -1
}

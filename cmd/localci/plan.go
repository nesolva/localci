package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/nesolva/localci/internal/workflow"
)

func init() {
	register("plan", "list the checks a pull request would run, and why others are skipped", false, cmdPlan)
}

const planLines = 10

// cmdPlan prints the checks a pull request would run, and why others are skipped.
func cmdPlan(args []string, stdout, stderr io.Writer) int {
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("localci plan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "branch the pull request targets (default: the repository's default branch)")
	head := fs.String("head", "HEAD", "commit the pull request is at")
	event := fs.String("event", "pull_request", "event to plan for: pull_request or push")
	asJSON := fs.Bool("json", false, "print one JSON object per line: a check or a skip")
	all := fs.Bool("all", false, "list every item instead of the first 10 of each kind")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: localci plan [dir] [--base main] [--head HEAD] [--json] [--all]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 && dir == "." {
		dir = fs.Arg(0)
	}
	if *event != "pull_request" && *event != "push" {
		fmt.Fprintf(stderr, "localci plan: unknown event %q (use pull_request or push)\n", *event)
		return 2
	}
	if *base == "" {
		*base = defaultBranch(dir)
	}

	baseRef := *base
	if exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}").Run() != nil {
		baseRef = "origin/" + *base
	}
	res, err := workflow.PlanDetailed(dir, *head, workflow.Event{
		Name:    *event,
		Branch:  strings.TrimPrefix(*base, "origin/"),
		BaseRef: baseRef,
	})
	if err != nil {
		fmt.Fprintln(stderr, "localci plan:", err)
		return 3
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, c := range res.Checks {
			enc.Encode(struct {
				Type string `json:"type"`
				workflow.Check
			}{"check", c})
		}
		for _, s := range res.Skipped {
			enc.Encode(struct {
				Type string `json:"type"`
				workflow.Skip
			}{"skip", s})
		}
		return 0
	}

	fmt.Fprintf(stdout, "%s: %d checks would run, %d skipped (%s...%s)\n", *event, len(res.Checks), len(res.Skipped), *base, *head)
	for i, c := range res.Checks {
		if i == planLines && !*all {
			fmt.Fprintf(stdout, "... %d more checks (use --all)\n", len(res.Checks)-i)
			break
		}
		fmt.Fprintf(stdout, "run   %s / %s  [%d steps%s]%s\n", c.Workflow, c.Name, len(c.Steps), extras(c), unsupportedNote(c))
	}
	for i, s := range res.Skipped {
		if i == planLines && !*all {
			fmt.Fprintf(stdout, "... %d more skipped (use --all)\n", len(res.Skipped)-i)
			break
		}
		who := s.Workflow
		if s.Job != "" {
			who += " / " + s.Job
		}
		fmt.Fprintf(stdout, "skip  %s: %s\n", who, s.Reason)
	}
	return 0
}

func extras(c workflow.Check) string {
	var parts []string
	if c.Container != nil {
		parts = append(parts, "container "+c.Container.Image)
	}
	for _, s := range c.Services {
		parts = append(parts, "service "+s.Name)
	}
	for _, t := range []string{"node", "java", "python", "go"} {
		if v, ok := c.Toolchain[t]; ok {
			parts = append(parts, t+" "+v)
		}
	}
	if c.NeedsActions {
		parts = append(parts, "needs actions")
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func unsupportedNote(c workflow.Check) string {
	if c.Unsupported == "" {
		return ""
	}
	return "  UNSUPPORTED: " + c.Unsupported
}

// defaultBranch is the branch origin points at, "main" when that is unknown.
func defaultBranch(dir string) string {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		if b := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); b != "" {
			return b
		}
	}
	return "main"
}

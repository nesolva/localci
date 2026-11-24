package explore

import (
	"fmt"
	"strings"

	"github.com/nesolva/localci/internal/flow"
)

// FlowLines is `localci flows` for the brief: one line per flow, with what it takes and returns.
func FlowLines(fs []flow.Summary) []string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s (in: %s; out: %s)", Text(f.Name, 80), keys(f.Inputs), keys(f.Outputs)))
		if len(out) == 20 {
			break
		}
	}
	return out
}

func keys(m map[string]any) string {
	if len(m) == 0 {
		return "-"
	}
	var ks []string
	for k := range m {
		ks = append(ks, Text(k, 40))
	}
	sortStrings(ks)
	return strings.Join(ks, ", ")
}

// Brief is what the agent reads on its standard input.
type Brief struct {
	Goals    []string
	BaseURL  string
	Findings string // $LOCALCI_FINDINGS
	OutDir   string // $LOCALCI_OUTPUT_DIR
	Minutes  int
	Flows    []string
}

// String renders the brief.
func (b Brief) String() string {
	var s strings.Builder
	s.WriteString("# Explore this app\n\n")
	s.WriteString("You are testing a running web app the way a careful new user would, looking for what is broken, confusing or\n")
	s.WriteString("inconsistent. You do not change code and you do not write to the repository. The app runs on its own copy of\n")
	s.WriteString("the database, so it is safe to create, change and delete data.\n\n")
	fmt.Fprintf(&s, "App: %s\n", b.BaseURL)
	fmt.Fprintf(&s, "Time: about %d minutes, then you are stopped. What you reported by then is kept, so report each finding the\n", b.Minutes)
	s.WriteString("moment you have it.\n\n## Goals\n\nTry to reach each goal. Notice everything that goes wrong on the way, not only whether you got there.\n\n")
	for i, g := range b.Goals {
		fmt.Fprintf(&s, "%d. %s\n", i+1, g)
	}
	if len(b.Flows) > 0 {
		s.WriteString("\n## Flows the team's tests already use (`localci flows`)\n\n")
		s.WriteString("Their tests cover these paths; spend your time on what they do not reach.\n\n")
		for _, f := range b.Flows {
			fmt.Fprintf(&s, "- %s\n", f)
		}
	}
	s.WriteString("\n## How to drive the app\n\n")
	s.WriteString("Your working directory is the test repository, so Playwright is installed: write small scripts and run them with\n")
	s.WriteString("`npx playwright` or node, or use any browser tool you have. Look at the page (an accessibility snapshot, a\n")
	s.WriteString("screenshot) before and after every step. Check the browser console and the network for errors too.\n")
	s.WriteString("Do the work yourself, one command at a time, and wait for each to finish: do not start background commands or\n")
	s.WriteString("hand work to sub-agents, because this session ends the moment you stop replying and takes them with it.\n\n")
	s.WriteString("## How to report\n\n")
	fmt.Fprintf(&s, "Append one JSON object per line to `%s` (also `$LOCALCI_FINDINGS`), nothing else in that file:\n\n", b.Findings)
	s.WriteString("```\n{\"goal\":\"which goal you were on\",\"severity\":\"high|medium|low\",\"title\":\"one line: what is wrong\",\n")
	s.WriteString(" \"steps\":[\"exact steps to see it, one per item\"],\"screenshot\":\"file name inside the output directory\",\n")
	s.WriteString(" \"test\":\"optional: a Playwright test in TypeScript that fails while the bug exists\"}\n```\n\n")
	fmt.Fprintf(&s, "Save screenshots (PNG) in `%s` (also `$LOCALCI_OUTPUT_DIR`) and name them relative to it. `high` is a crash, data loss\n", b.OutDir)
	s.WriteString("or a goal that cannot be reached; `medium` is wrong behaviour with a way around; `low` is polish. Report a problem\n")
	s.WriteString("once, with steps someone else can follow. Do not report what you only suspect without trying it twice.\n")
	return s.String()
}

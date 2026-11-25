package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/affected"
	"github.com/nesolva/localci/internal/server"
)

func init() {
	register("run", "run a suite (or the checks) on this checkout's working tree", true,
		agent("run", "[--suite s | --checks] [--grep re] [--failed <id>] [--affected [--base b] [--explain]] [--wait] [files…]\n\nThe run gets the server's environment (`localci serve`, or the service when installed), not the one of the shell that ran this command. Pass values to the tests through the suite's `env:`.", func(fs *flag.FlagSet) {
			fs.String("suite", "", "suite to run (default: the only one configured)")
			fs.Bool("checks", false, "run the pull request checks of this checkout instead of a suite")
			fs.String("grep", "", "only tests whose file and title match this regular expression")
			fs.String("failed", "", "only the tests that failed in this run")
			fs.Bool("affected", false, "only the tests the diff can affect (all of them when that is not safe)")
			fs.String("base", "", "with --affected: the branch the diff starts from (default: the default branch)")
			fs.Bool("explain", false, "with --affected: choose the tests, run none, and say why each one (or why all)")
			fs.Bool("wait", false, "wait for the run and print how it ended")
			fs.Duration("timeout", defaultWait, "with --wait: give up after this long (exit 4)")
			fs.Bool("force", false, "with --checks: run even when this tree already passed")
		}, cmdRun))
}

const defaultWait = 30 * time.Minute

func cmdRun(e *env, files []string) int {
	get := func(n string) flag.Value { return e.fs.Lookup(n).Value }
	cwd, err := os.Getwd()
	if err != nil {
		return e.fail(err)
	}
	req := server.SubmitRequest{
		Suite: get("suite").String(), Grep: get("grep").String(), Failed: get("failed").String(),
		Checks: get("checks").String() == "true", Force: get("force").String() == "true",
		Dir: cwd, Files: repoRelative(cwd, files),
		Affected: get("affected").String() == "true", Base: get("base").String(), Explain: get("explain").String() == "true",
	}
	if (req.Base != "" || req.Explain) && !req.Affected {
		fmt.Fprintln(e.err, "localci: --base and --explain go with --affected")
		return exitUsage
	}
	if req.Affected && req.Checks {
		fmt.Fprintln(e.err, "localci: --affected chooses suite tests; --checks runs the pull request checks")
		return exitUsage
	}
	if req.Checks && (req.Suite != "" || req.Grep != "" || req.Failed != "" || len(files) > 0) {
		fmt.Fprintln(e.err, "localci: --checks runs the pull request checks; --suite, --grep, --failed and files are for suites")
		return exitUsage
	}
	var res server.SubmitResult
	if err := e.c.post("/api/runs", req, &res); err != nil {
		return e.fail(err)
	}
	for _, r := range res.Runs {
		if e.json {
			e.emit("run", r)
		} else {
			fmt.Fprintln(e.out, runLine(r))
		}
	}
	for i, s := range res.Skipped {
		if e.json {
			e.emit("skipped", s)
			continue
		}
		if i == listLines {
			fmt.Fprintf(e.out, "%d more skipped\n", len(res.Skipped)-i)
			break
		}
		fmt.Fprintf(e.out, "skipped %s/%s: %s\n", s.Workflow, s.Job, s.Reason)
	}
	if len(res.Runs) == 0 {
		fmt.Fprintln(e.err, "localci: nothing to run: no check applies to this tree")
		return exitOK
	}
	if get("wait").String() != "true" && !req.Explain {
		return exitOK
	}
	d, _ := time.ParseDuration(get("timeout").String())
	ids := make([]string, len(res.Runs))
	for i, r := range res.Runs {
		ids[i] = r.ID
	}
	code := waitFor(e, ids, d)
	if req.Explain && code == exitOK {
		return explain(e, ids)
	}
	return code
}

// repoRelative makes test paths relative to the repository root, as the runner expects, when the command was
// run from a subdirectory.
func repoRelative(cwd string, files []string) []string {
	if len(files) == 0 {
		return nil
	}
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-prefix").Output()
	prefix := strings.TrimSpace(string(out))
	res := make([]string, len(files))
	for i, f := range files {
		if err != nil || prefix == "" || filepath.IsAbs(f) {
			res[i] = f
			continue
		}
		res[i] = path.Join(prefix, filepath.ToSlash(f))
	}
	return res
}

// explain prints the choice of the explain-only runs: why each test, or why all of them.
func explain(e *env, ids []string) int {
	for _, id := range ids {
		var sel affected.Selection
		if err := e.c.get("/api/runs/"+id+"/affected", &sel); err != nil {
			return e.fail(err)
		}
		if e.json {
			e.emit("affected", sel)
		} else {
			fmt.Fprint(e.out, affected.Explain(sel))
		}
	}
	return exitOK
}

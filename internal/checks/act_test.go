package checks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

const marketplaceWorkflow = `name: Deploy preview
on: [pull_request]
jobs:
  preview:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        region: [eu]
    steps:
      - uses: actions/checkout@v4
      - uses: acme/preview-action@v2
        with:
          region: ${{ matrix.region }}
`

// fakeAct puts an `act` on PATH that records its arguments and event file.
func fakeAct(t *testing.T, exit int) (record string) {
	return fakeActOutput(t, exit, "")
}

// fakeActOutput is fakeAct with act's last line chosen by the test; "" is what
// a real act prints for a job that ran: its success or failure line.
func fakeActOutput(t *testing.T, exit int, last string) (record string) {
	t.Helper()
	if last == "" {
		last = "[CI/preview] Job succeeded"
		if exit != 0 {
			last = "[CI/preview] Job failed"
		}
	}
	dockerUp = func() bool { return true }
	t.Cleanup(func() { dockerUp = exec.DockerAvailable })
	bin := t.TempDir()
	record = filepath.Join(t.TempDir(), "act-call")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + record + `.args"
while [ $# -gt 0 ]; do [ "$1" = "-e" ] && cp "$2" "` + record + `.event"; shift; done
echo "act ran the preview"
echo "` + last + `"
exit ` + string(rune('0'+exit)) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "act"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return record
}

func TestActJobRunsWithTheMatrixCellAndPullRequestPayload(t *testing.T) {
	record := fakeAct(t, 0)
	fx := newFixture(t, marketplaceWorkflow)
	fx.runner.Act = RunAct
	head := fx.pull("7", map[string]string{"feature.txt": "x\n"})

	sub := fx.submit(fx.request(7, head))
	if len(sub.Runs) != 1 {
		t.Fatalf("runs = %d, skipped %v", len(sub.Runs), sub.Skipped)
	}
	id := sub.Runs[0].RunID
	if st := fx.run(id).State; st != store.StatePassed {
		t.Fatalf("state %s\n%s", st, fx.runLog(id))
	}
	if !strings.Contains(fx.runLog(id), "act ran the preview") {
		t.Errorf("act output must reach the run log:\n%s", fx.runLog(id))
	}

	args, _ := os.ReadFile(record + ".args")
	got := strings.Fields(string(args))
	want := []string{"pull_request", "-j", "preview", "-W", ".github/workflows/ci.yml", "-e"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("args = %q, want prefix %q", got, want)
		}
	}
	if !strings.Contains(string(args), "--matrix\nregion:eu") {
		t.Errorf("matrix cell not passed: %q", args)
	}

	raw, err := os.ReadFile(record + ".event")
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Number      int `json:"number"`
		PullRequest struct {
			Head struct{ Sha string } `json:"head"`
			Base struct{ Ref string } `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Number != 7 || ev.PullRequest.Head.Sha != head || ev.PullRequest.Base.Ref != "main" {
		t.Errorf("payload = %s", raw)
	}
}

func TestActFailureFailsTheCheck(t *testing.T) {
	fakeAct(t, 1)
	fx := newFixture(t, marketplaceWorkflow)
	fx.runner.Act = RunAct
	head := fx.pull("8", map[string]string{"feature.txt": "x\n"})

	id := fx.submit(fx.request(8, head)).Runs[0].RunID
	if r := fx.run(id); r.State != store.StateFailed || !strings.Contains(r.Reason, "act job failed") {
		t.Errorf("run = %s %q", r.State, r.Reason)
	}
}

func TestActExitZeroWithoutAJobRunFailsTheCheck(t *testing.T) {
	fakeActOutput(t, 0, "level=warning msg=no matching job")
	fx := newFixture(t, marketplaceWorkflow)
	fx.runner.Act = RunAct
	head := fx.pull("10", map[string]string{"feature.txt": "x\n"})

	id := fx.submit(fx.request(10, head)).Runs[0].RunID
	if r := fx.run(id); r.State != store.StateFailed || !strings.Contains(r.Reason, "act ran no job") {
		t.Errorf("run = %s %q", r.State, r.Reason)
	}
}

func TestDockerDownIsAnInfrastructureErrorNotAFailedCheck(t *testing.T) {
	fakeAct(t, 0)
	dockerUp = func() bool { return false }
	fx := newFixture(t, marketplaceWorkflow)
	fx.runner.Act = RunAct
	head := fx.pull("11", map[string]string{"feature.txt": "x\n"})

	started := fx.submit(fx.request(11, head)).Runs[0]
	r := fx.run(started.RunID)
	if r.State != store.StateFailed || !strings.HasPrefix(r.Reason, "infrastructure:") || !strings.Contains(r.Reason, "Docker is not running") {
		t.Errorf("run = %s %q", r.State, r.Reason)
	}
	posts := fx.forge.of(strings.TrimPrefix(started.Context, "localci/"))
	if len(posts) == 0 || posts[len(posts)-1].State != forge.StateError {
		t.Errorf("statuses = %+v, want the last one an error", posts)
	}
}

func TestActTimeoutIsReportedAsTimedOut(t *testing.T) {
	fakeAct(t, 0)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "act"), []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	fx := newFixture(t, strings.Replace(marketplaceWorkflow, "    steps:", "    timeout-minutes: 1\n    steps:", 1))
	fx.runner.Act = func(ctx context.Context, j ActJob) (exec.Result, error) {
		j.Check.Timeout = 300 * time.Millisecond
		return RunAct(ctx, j)
	}
	head := fx.pull("12", map[string]string{"feature.txt": "x\n"})

	id := fx.submit(fx.request(12, head)).Runs[0].RunID
	if r := fx.run(id); r.State != store.StateFailed || !strings.HasPrefix(r.Reason, "timed out after") {
		t.Errorf("run = %s %q", r.State, r.Reason)
	}
}

func TestMissingActFailsTheCheckWithHowToInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+"/usr/bin:/bin")
	fx := newFixture(t, marketplaceWorkflow)
	fx.runner.Act = RunAct
	head := fx.pull("9", map[string]string{"feature.txt": "x\n"})

	id := fx.submit(fx.request(9, head)).Runs[0].RunID
	r := fx.run(id)
	if r.State != store.StateFailed || !strings.Contains(r.Reason, "brew install act") {
		t.Errorf("run = %s %q", r.State, r.Reason)
	}
}

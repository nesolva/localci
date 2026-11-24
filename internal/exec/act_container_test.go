package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// act applies --container-options only to a job without `container:`; a job with one carries no label unless
// its own container block says so. Every shape of that block must end up with the labels, and a workflow
// without containers must be left exactly as it is.
func TestContainerJobsGetTheLabelsInTheirOwnOptions(t *testing.T) {
	const labels = "--label localci.act=abc --label localci.run=r1"
	src := `name: CI
on: pull_request
jobs:
  short:
    runs-on: ubuntu-latest
    container: node:22
    steps: [{run: "true"}]
  full:
    runs-on: ubuntu-latest
    container:
      image: node:22
      env: {A: "1"}
    steps: [{run: "true"}]
  own:
    runs-on: ubuntu-latest
    container:
      image: node:22
      options: --cpus 1
    steps: [{run: "true"}]
  plain:
    runs-on: ubuntu-latest
    steps: [{run: "true"}]
`
	out, changed, err := addContainerLabels([]byte(src), labels)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var doc struct {
		Jobs map[string]struct {
			Container struct {
				Image   string            `yaml:"image"`
				Env     map[string]string `yaml:"env"`
				Options string            `yaml:"options"`
			} `yaml:"container"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for job, want := range map[string]string{"short": labels, "full": labels, "own": "--cpus 1 " + labels, "plain": ""} {
		c := doc.Jobs[job].Container
		if c.Options != want {
			t.Errorf("%s: options = %q, want %q", job, c.Options, want)
		}
		if job != "plain" && c.Image != "node:22" {
			t.Errorf("%s: image = %q, want it kept", job, c.Image)
		}
	}
	if doc.Jobs["full"].Container.Env["A"] != "1" {
		t.Error("the container's other settings were lost")
	}

	plain := "name: CI\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps: [{run: x}]\n"
	if _, changed, err := addContainerLabels([]byte(plain), labels); changed || err != nil {
		t.Errorf("a workflow without containers must stay as it is: changed=%v err=%v", changed, err)
	}
}

// The labelled copy is what act runs, beside the log and never in the workspace; a workflow without a container
// job is run from the workspace as before.
func TestActRunsALabelledCopyOnlyForContainerJobs(t *testing.T) {
	for _, tc := range []struct {
		name, workflow string
		copied         bool
	}{
		{"container job", "name: CI\njobs:\n  t:\n    runs-on: ubuntu-latest\n    container: node:22\n    steps: [{run: x}]\n", true},
		{"no container", "name: CI\njobs:\n  t:\n    runs-on: ubuntu-latest\n    steps: [{run: x}]\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := actSpecIn(t)
			spec.Run = "r1"
			wfFile := filepath.Join(spec.Dir, ".github", "workflows", "ci.yml")
			if err := os.WriteFile(wfFile, []byte(tc.workflow), 0o644); err != nil {
				t.Fatal(err)
			}
			seen := filepath.Join(t.TempDir(), "seen")
			// the fake act copies the workflow it was asked to run
			fakeActBin(t, `while [ $# -gt 0 ]; do [ "$1" = -W ] && cp "$2" "`+seen+`"; shift; done; echo "[CI/t] Job succeeded"`)
			if _, err := RunAct(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			ran, _ := os.ReadFile(seen)
			if got := strings.Contains(string(ran), "localci.run=r1"); got != tc.copied {
				t.Errorf("act ran a labelled workflow = %v, want %v:\n%s", got, tc.copied, ran)
			}
			if b, _ := os.ReadFile(wfFile); string(b) != tc.workflow {
				t.Error("the workflow in the workspace was changed")
			}
		})
	}
}

// act drops --container-options for a job with `container:`, so that job's container used to carry no label and
// the sweep at the next start never found it. With real act: the job container of a run carries the run's label,
// and the sweep removes it. Needs Docker, act and an image with `sh` and `ping` (LOCALCI_TEST_IMAGE; act also
// probes the image for `node`).
func TestActJobWithAContainerIsFoundAndRemovedBySweep(t *testing.T) {
	img := dockerImage(t)
	if _, err := osexec.LookPath("act"); err != nil {
		t.Skip("act is not installed")
	}
	b := make([]byte, 4)
	rand.Read(b)
	run := "t" + hex.EncodeToString(b)
	listRun := func() string {
		out, _ := osexec.Command("docker", "ps", "-a", "-q", "--filter", "label="+RunLabelKey+"="+run).Output()
		return strings.TrimSpace(string(out))
	}
	t.Cleanup(func() {
		if ids := strings.Fields(listRun()); len(ids) > 0 {
			_ = osexec.Command("docker", append([]string{"rm", "-f", "-v"}, ids...)...).Run()
		}
	})

	spec := actSpecIn(t)
	spec.Run, spec.Matrix = run, nil
	wf := "name: CI\non: pull_request\njobs:\n  t:\n    runs-on: ubuntu-latest\n    container: " + img +
		"\n    steps:\n      - run: ping -c 120 127.0.0.1\n"
	if err := os.WriteFile(filepath.Join(spec.Dir, ".github", "workflows", "ci.yml"), []byte(wf), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := osexec.Command("git", "-C", spec.Dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _, _ = RunAct(ctx, spec) }()

	deadline := time.Now().Add(90 * time.Second)
	for listRun() == "" {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("no container carries the run's label: act dropped it for the job with a container")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if n := SweepRunContainers(context.Background(), func(r string) bool { return r == run }); n != 1 {
		t.Errorf("the sweep removed %d job containers, want 1", n)
	}
	if left := listRun(); left != "" {
		t.Errorf("the job container is still there after the sweep: %s", left)
	}
	cancel()
	<-done
	if left := listRun(); left != "" {
		t.Errorf("a container is left after the run ended: %s", left)
	}
}

package checks

import (
	"fmt"
	"os"
	osexec "os/exec"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
)

// bashImage is an image that has bash (LOCALCI_TEST_BASH_IMAGE, default ubuntu:24.04), present locally: on an image
// without bash the container cases would pass whatever the flags are.
func bashImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("LOCALCI_TEST_BASH_IMAGE")
	if img == "" {
		img = "ubuntu:24.04"
	}
	if !exec.DockerAvailable() {
		t.Skip("Docker is not running")
	}
	if osexec.Command("docker", "image", "inspect", img).Run() != nil {
		t.Skipf("image %s is not present locally (set LOCALCI_TEST_BASH_IMAGE to one that is)", img)
	}
	if osexec.Command("docker", "run", "--rm", "--entrypoint", "sh", img, "-c", "command -v bash").Run() != nil {
		t.Skipf("image %s has no bash", img)
	}
	return img
}

// A step without `shell:` runs as GitHub runs it, `bash -e`: no pipefail. `shell: bash` adds it.
func TestStepDefaultShell(t *testing.T) {
	run := func(t *testing.T, container, shell string) store.Run {
		t.Helper()
		var cont, sh string
		if container != "" {
			cont = "    container: " + container + "\n"
		}
		if shell != "" {
			sh = "        shell: " + shell + "\n"
		}
		wf := fmt.Sprintf("name: CI\non: [pull_request]\njobs:\n  j:\n    runs-on: ubuntu-latest\n%s    steps:\n      - run: false | true\n%s", cont, sh)
		fx := newFixture(t, wf)
		r := fx.submit(fx.request(9, fx.pull("9", map[string]string{"a": "1"}))).Runs[0]
		st := fx.run(r.RunID)
		t.Logf("%s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
		return st
	}
	for _, where := range []string{"host", "container"} {
		t.Run(where, func(t *testing.T) {
			image := ""
			if where == "container" {
				image = bashImage(t)
			}
			if st := run(t, image, ""); st.State != store.StatePassed {
				t.Errorf("no shell: %s %s, want passed (bash -e has no pipefail)", st.State, st.Reason)
			}
			if st := run(t, image, "bash"); st.State != store.StateFailed {
				t.Errorf("shell: bash: %s, want failed (pipefail)", st.State)
			}
		})
	}
}

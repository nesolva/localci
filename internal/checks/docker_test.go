package checks

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
)

func dockerImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("LOCALCI_TEST_IMAGE")
	if img == "" {
		img = "alpine:3"
	}
	if !exec.DockerAvailable() {
		skipDocker(t, "Docker is not running")
	}
	if osexec.Command("docker", "image", "inspect", img).Run() != nil {
		skipDocker(t, "image %s is not present locally (set LOCALCI_TEST_IMAGE to one that is)", img)
	}
	return img
}

// skipDocker skips a test that needs Docker or an image this machine lacks. CI sets LOCALCI_TEST_DOCKER=require
// after pulling the images, and there it fails instead, so a Docker test cannot pass in CI by not running.
func skipDocker(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("LOCALCI_TEST_DOCKER") == "require" {
		t.Fatalf("LOCALCI_TEST_DOCKER=require: "+format, args...)
	}
	t.Skipf(format, args...)
}

// docker lists what is left of a run: names of containers and networks.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := osexec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestServicesAreReachableByAliasAndRemovedAfter(t *testing.T) {
	img := dockerImage(t)
	serve := filepath.Join(t.TempDir(), "serve.sh")
	if err := os.WriteFile(serve, []byte("#!/bin/sh\nwhile true; do printf 'HTTP/1.0 200 OK\\r\\n\\r\\npong\\n' | nc -l -p 8080; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wf := fmt.Sprintf(`name: CI
on: [pull_request]
jobs:
  api:
    runs-on: ubuntu-latest
    services:
      web:
        image: %s
        volumes:
          - %s:/serve.sh:ro
        options: --entrypoint /serve.sh
    steps:
      - name: call the service
        run: |
          for i in $(seq 40); do wget -qO- http://web:8080/ && exit 0; sleep 0.25; done
          exit 1
`, img, serve)
	fx := newFixture(t, wf)
	fx.runner.Options.DefaultImage = img
	head := fx.pull("9", map[string]string{"a": "1"})
	r := fx.submit(fx.request(9, head)).Runs[0]

	if st := fx.run(r.RunID); st.State != store.StatePassed {
		t.Fatalf("state = %s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
	}
	if log := fx.runLog(r.RunID); !strings.Contains(log, "pong") {
		t.Fatalf("the job did not get the service's answer:\n%s", log)
	}
	name := "localci-" + r.RunID
	if left := docker(t, "ps", "-a", "--filter", "name="+name, "-q"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
	if left := docker(t, "network", "ls", "--filter", "name="+name, "-q"); left != "" {
		t.Fatalf("network left behind: %s", left)
	}
}

func TestContainerJobSeesTheWorkspaceAtTheSamePath(t *testing.T) {
	img := dockerImage(t)
	wf := fmt.Sprintf(`name: CI
on: [pull_request]
jobs:
  inside:
    runs-on: ubuntu-latest
    container: %s
    env:
      GREETING: hello
    steps:
      - run: test "$(cat feature.txt)" = x && test "$PWD" = "$GITHUB_WORKSPACE" && echo "$GREETING from $(pwd)"
      - run: echo made-in-container > made.txt
`, img)
	fx := newFixture(t, wf)
	head := fx.pull("10", map[string]string{"feature.txt": "x"})
	r := fx.submit(fx.request(10, head)).Runs[0]
	if st := fx.run(r.RunID); st.State != store.StatePassed {
		t.Fatalf("state = %s %s\n%s", st.State, st.Reason, fx.runLog(r.RunID))
	}
	if !strings.Contains(fx.runLog(r.RunID), "hello from /") {
		t.Fatalf("log:\n%s", fx.runLog(r.RunID))
	}
	if left := docker(t, "ps", "-a", "--filter", "name=localci-"+r.RunID, "-q"); left != "" {
		t.Fatalf("containers left behind: %s", left)
	}
}

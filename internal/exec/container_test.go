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
)

func TestRewriteDockerRun(t *testing.T) {
	const cache = " -e XDG_CACHE_HOME=/localci-cache -v localci-cache-"
	cases := []struct{ name, in, want string }{
		{"plain",
			"docker run --rm aquasec/trivy:0.50 image app:scan",
			"docker run" + cache + "aquasec-trivy-0.50:/localci-cache --rm aquasec/trivy:0.50 image app:scan"},
		{"flags with values come before the image",
			"docker run --rm -v \"$PWD:/src\" -e TOKEN=x --network host scanner:1 scan /src",
			"docker run" + cache + "scanner-1:/localci-cache --rm -v \"$PWD:/src\" -e TOKEN=x --network host scanner:1 scan /src"},
		{"continued lines",
			"docker run --rm \\\n  -v /a:/b \\\n  scanner:1 scan",
			"docker run" + cache + "scanner-1:/localci-cache --rm \\\n  -v /a:/b \\\n  scanner:1 scan"},
		{"every run in a script, not builds",
			"docker build -t app:scan .\ndocker run scanner:1 a && docker run other:2 b",
			"docker build -t app:scan .\ndocker run" + cache + "scanner-1:/localci-cache scanner:1 a && docker run" + cache + "other-2:/localci-cache other:2 b"},
		{"an image from a variable has no stable volume",
			"docker run --rm $IMAGE scan", "docker run --rm $IMAGE scan"},
		{"a run that sets its own cache is left alone",
			"docker run -e XDG_CACHE_HOME=/c scanner:1", "docker run -e XDG_CACHE_HOME=/c scanner:1"},
		{"the container's own command may mention the variable",
			"docker run scanner:1 sh -c 'ls $XDG_CACHE_HOME'",
			"docker run" + cache + "scanner-1:/localci-cache scanner:1 sh -c 'ls $XDG_CACHE_HOME'"},
		{"a word that only ends in docker", "mydocker run x", "mydocker run x"},
	}
	for _, c := range cases {
		if got := RewriteDockerRun(c.in); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// ExecLine must hand the script to the container as one argument, quotes and all.
func TestExecLineKeepsTheScriptInOneArgument(t *testing.T) {
	bin := t.TempDir()
	out := filepath.Join(t.TempDir(), "args")
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + out + "; done\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	script := "echo \"it's $HOME\"\nls -l | wc -l"
	line := ExecLine("job1", "/w/sub dir", "bash", map[string]string{"B": "two words", "A": "1"}, script)
	if err := osexec.Command("bash", "-c", line).Run(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{"exec", "-w", "/w/sub dir", "-e", "A=1", "-e", "B=two words", "job1", "bash", "--noprofile", "--norc", "-eo", "pipefail", "-c"}
	if strings.Join(got[:len(want)], "|") != strings.Join(want, "|") || strings.Join(got[len(want):], "\n") != script {
		t.Fatalf("docker got %q", got)
	}
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

func dockerImage(t *testing.T) string {
	t.Helper()
	img := os.Getenv("LOCALCI_TEST_IMAGE")
	if img == "" {
		img = "alpine:3"
	}
	if !DockerAvailable() {
		skipDocker(t, "Docker is not running")
	}
	if osexec.Command("docker", "image", "inspect", img).Run() != nil {
		skipDocker(t, "image %s is not present locally (set LOCALCI_TEST_IMAGE to one that is)", img)
	}
	return img
}

// A scanner's cache volume must still be there for the next run.
func TestCacheVolumePersistsBetweenRuns(t *testing.T) {
	img := dockerImage(t)
	var b [4]byte
	rand.Read(b[:])
	tag := "localci-test-" + hex.EncodeToString(b[:]) + ":1"
	if err := osexec.Command("docker", "tag", img, tag).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		osexec.Command("docker", "volume", "rm", "-f", CacheVolume(tag)).Run()
		osexec.Command("docker", "rmi", tag).Run()
	})

	script := RewriteDockerRun("docker run --rm " + tag + " sh -c 'cat \"$XDG_CACHE_HOME/db\" 2>/dev/null; echo downloaded >> \"$XDG_CACHE_HOME/db\"'")
	run := func(name string) string {
		log := filepath.Join(t.TempDir(), name+".log")
		res, err := Run(context.Background(), Spec{Cmd: script, Dir: t.TempDir(), LogPath: log})
		if err != nil || !res.OK() {
			t.Fatalf("run %s: %v %+v", name, err, res)
		}
		out, _ := os.ReadFile(log)
		return strings.TrimSpace(string(out))
	}
	if first := run("first"); first != "" {
		t.Fatalf("the first run should find an empty cache, got %q", first)
	}
	if second := run("second"); second != "downloaded" {
		t.Fatalf("the second run should find the first one's cache, got %q", second)
	}
}

// A killed run leaves its act job container (the only one labelled) and the service containers on the network
// act made for the job. The next start removes those of runs that ended; a run that is still running, or that
// this store does not know, keeps its containers.
func TestSweepRemovesTheContainersOfFinishedRuns(t *testing.T) {
	img := dockerImage(t)
	ctx := context.Background()
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	docker := func(args ...string) {
		t.Helper()
		if out, err := osexec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
	}
	exists := func(kind, name string) bool { return osexec.Command("docker", kind, "inspect", name).Run() == nil }
	// job makes the job container of a run and, like act, a service container on a network named after it.
	job := func(tag, run string) (jobName, svcName, net string) {
		jobName, svcName, net = "act-ci-t-"+id+tag, "act-svc-"+id+tag, "act-ci-t-"+id+tag+"-t-network"
		docker("network", "create", net)
		t.Cleanup(func() {
			_ = osexec.Command("docker", "rm", "-f", "-v", jobName, svcName).Run()
			_ = osexec.Command("docker", "network", "rm", net).Run()
		})
		docker("run", "-d", "--name", jobName, "--network", net, "--label", RunLabelKey+"="+run, img, "sleep", "300")
		docker("run", "-d", "--name", svcName, "--network", net, img, "sleep", "300")
		return
	}
	endedJob, endedSvc, endedNet := job("a", "ended-"+id)
	runningJob, runningSvc, _ := job("b", "running-"+id)
	unknownJob, _, _ := job("c", "unknown-"+id)

	if n := SweepRunContainers(ctx, func(run string) bool { return run == "ended-"+id }); n != 1 {
		t.Fatalf("removed %d job containers, want 1", n)
	}
	if exists("container", endedJob) || exists("container", endedSvc) || exists("network", endedNet) {
		t.Error("the job container, its service container or its network is still there")
	}
	if !exists("container", runningJob) || !exists("container", runningSvc) || !exists("container", unknownJob) {
		t.Error("the sweep removed the containers of a run that has not ended or is not known")
	}
}

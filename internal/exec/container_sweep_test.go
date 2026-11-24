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

// act names its containers by workflow and job, so a name outlives the container that had it: when a stale
// container is replaced by a newer live job between the sweep listing it and removing it, removing "by name"
// kills the live job. The sweep must remove the container it listed, by ID.
func TestSweepNeverRemovesALiveContainerThatTookOverAStaleOnesName(t *testing.T) {
	img := dockerImage(t)
	real, err := osexec.LookPath("docker")
	if err != nil {
		t.Skip("docker not on PATH")
	}
	b := make([]byte, 4)
	rand.Read(b)
	id := hex.EncodeToString(b)
	name := "act-ci-t-" + id
	run := func(args ...string) {
		t.Helper()
		if out, err := osexec.Command(real, args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
	}
	t.Cleanup(func() { _ = osexec.Command(real, "rm", "-f", "-v", name).Run() })
	run("run", "-d", "--name", name, "--label", RunLabelKey+"=ended-"+id, img, "ping", "-c", "300", "127.0.0.1")

	// A docker that, right after the first listing, replaces the stale container by a live one of another run
	// with the same name: the window between the sweep's listing and its removal.
	shim := t.TempDir()
	marker := filepath.Join(shim, "swapped")
	script := `#!/bin/sh
out=$("` + real + `" "$@"); rc=$?
printf '%s\n' "$out"
if [ "$1" = ps ] && [ ! -e "` + marker + `" ]; then
  : > "` + marker + `"
  "` + real + `" rm -f -v "` + name + `" >/dev/null 2>&1
  "` + real + `" run -d --name "` + name + `" --label ` + RunLabelKey + `=live-` + id + ` ` + img + ` ping -c 300 127.0.0.1 >/dev/null 2>&1
fi
exit $rc
`
	if err := os.WriteFile(filepath.Join(shim, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))

	SweepRunContainers(context.Background(), func(run string) bool { return run == "ended-"+id })

	out, _ := osexec.Command(real, "inspect", "-f", `{{.State.Running}} {{index .Config.Labels "`+RunLabelKey+`"}}`, name).CombinedOutput()
	if got := strings.TrimSpace(string(out)); got != "true live-"+id {
		t.Fatalf("the live container that took the stale one's name is gone or changed: %q", got)
	}
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/server"
)

// The CLI and a real `serve` on the real sample (Playwright package, example shop app): the CLI reaches the server
// only over its socket, the shards' hook calls reach it over TCP with their run's token, and run --wait, show and
// run --failed work as an agent uses them. Needs `npm install` at the root and `npm run build -w packages/playwright`.
func TestCLIAgainstARunningServeOnTheSample(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{"node_modules/.bin/localci-playwright", "packages/playwright/dist/cli.js", "examples/acme-shop/package.json"} {
		if _, err := os.Stat(filepath.Join(root, need)); err != nil {
			t.Skipf("sample suite needs %s (npm install; npm run build -w packages/playwright)", need)
		}
	}
	sh := func(dir string, args ...string) {
		t.Helper()
		cmd := osexec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	src := t.TempDir()
	if err := os.CopyFS(src, os.DirFS(filepath.Join(root, "examples", "acme-shop"))); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(src, "node_modules"))
	sh(src, "git", "init", "-q", "-b", "main")
	sh(src, "git", "remote", "add", "origin", "https://github.com/acme/shop.git")
	sh(src, "git", "add", "-A")
	sh(src, "git", "-c", "user.name=t", "-c", "user.email=t@localhost", "commit", "-q", "-m", "sample")

	// The server clones acme/shop from its public address; point that at the local copy.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+src+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/shop.git")

	// short: the socket lives under it; a space, like macOS's "Application Support", so unquoted paths break here
	home, err := os.MkdirTemp("", "l c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("LOCALCI_SOCKET", "")
	dataDir := config.DataDir()

	cfgFile := filepath.Join(home, "config.yaml")
	yaml := fmt.Sprintf(`suites:
  shop:
    repos: [acme/shop]
    prepare:
      - repo: acme/shop
        run: 'ln -sfn %s/node_modules node_modules'
        cache_key: [package.json]
        keep: [node_modules]
    database:  # the sample's (examples/acme-shop/localci.yaml)
      driver: command
      template: {seed: "echo '{\"tenants\":{}}' > '{{db.url}}'"}
      create: "cp {{template}} {{name}}"
      drop: "rm -f {{name}}"
    test:
      repo: acme/shop
      run: npx playwright test
      env:
        PORT: '{{port "web"}}'
        DB_PATH: "{{db.url}}"
        ACME_GLOBAL_SETTINGS: %q
        ACME_BREAK: "1"
    shards: 2
    timeout: 10m
`, root, filepath.Join(home, "global-settings.json"))
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	machine := capacity.Machine{CPUs: 4}
	eff, err := config.Load(config.Options{Path: cfgFile, Machine: machine})
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	var logs strings.Builder
	go func() {
		done <- server.Start(ctx, server.Options{Config: eff, DataDir: dataDir, Machine: machine, Port: -1, WorkerPort: -1,
			Log: log.New(&logs, "", 0), Ready: func(a string) { ready <- a }, Reload: func() (*config.Effective, error) { return eff, nil }})
	}()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("serve did not stop")
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve stopped: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not start")
	}

	t.Chdir(src)
	cmd := func(args ...string) (string, int) {
		t.Helper()
		var out, errb strings.Builder
		code := run(args, &out, &errb)
		if errb.Len() > 0 {
			t.Logf("localci %v stderr: %s", args, errb.String())
		}
		return out.String(), code
	}

	out, code := cmd("run", "--wait", "--json")
	if code != exitFailed {
		t.Fatalf("run --wait on the broken sample: exit %d, want 1\n%s\nserver log:\n%s", code, out, logs.String())
	}
	var runID string
	for _, line := range strings.Split(out, "\n") {
		var ev struct{ Type, ID string }
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "run" && ev.ID != "" {
			runID = ev.ID
		}
	}
	if runID == "" {
		t.Fatalf("no run id in the output:\n%s", out)
	}
	shown, code := cmd("show", runID)
	if code != exitOK || !strings.Contains(shown, "expected") {
		t.Errorf("show %s: exit %d\n%s", runID, code, shown)
	}
	out, code = cmd("run", "--failed", runID, "--wait")
	if code != exitFailed || !strings.Contains(out, "failed") {
		t.Errorf("run --failed %s: exit %d\n%s", runID, code, out)
	}
}

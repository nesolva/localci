package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/server"
)

// TestMain lets a test run the CLI as a separate process: the test binary itself, told to act as localci.
func TestMain(m *testing.M) {
	if os.Getenv("LOCALCI_TEST_AS_CLI") == "1" {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// cliProcess is `localci args…` as its own process in dir, with nothing of the real user's setup in reach.
func cliProcess(t *testing.T, dir string, args ...string) (*osexec.Cmd, *bytes.Buffer) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("# empty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command(os.Args[0], append(args, "--config", cfg, "--server-port", freePort(t))...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LOCALCI_TEST_AS_CLI=1", "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd, &stderr
}

// answers reports whether the server of dataDir answers on its API socket.
func answers(dataDir string) bool {
	sock, err := server.FindSocket(dataDir)
	if err != nil {
		return false
	}
	c := &http.Client{Timeout: time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	resp, err := c.Get("http://localhost/api/queue")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// socketPost makes a POST without a token over the socket of dataDir and returns the answer's body.
func socketPost(t *testing.T, dataDir, path string) string {
	t.Helper()
	sock, err := server.FindSocket(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	resp, err := c.Post("http://localhost"+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return b.String()
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A second serve on a data dir must not touch what the first one has: it used to dial the socket and, finding it
// busy or not, unlink it. It exits 3 saying who runs, and the first one keeps answering. A relative --data-dir
// names the same directory as its absolute spelling, and everything lives there.
func TestSecondServeOnTheSameDataDirExitsAndLeavesTheFirstAlone(t *testing.T) {
	root, err := os.MkdirTemp("", "lc") // short: the socket lives under it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, "rel", "dir")

	first, _ := cliProcess(t, root, "serve", "--data-dir", "rel/dir")
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Process.Signal(syscall.SIGTERM); first.Wait() })
	waitUntil(t, "the first serve to answer", func() bool { return answers(abs) })
	for _, f := range []string{"localci.db", "token", "serve.lock", server.SocketName} {
		if _, err := os.Stat(filepath.Join(abs, f)); err != nil {
			t.Errorf("a relative --data-dir must store %s under its absolute path: %v", f, err)
		}
	}

	// The server knows its data dir as the path it was started with: the refusal of a write without the token
	// names the token file, and that path is absolute only when --data-dir was made absolute.
	if body := socketPost(t, abs, "/api/runs/1/cancel"); !strings.Contains(body, filepath.Join(abs, "token")) {
		t.Errorf("the server does not hold the absolute data dir %s: %s", abs, body)
	}

	other := t.TempDir() // another working directory, the absolute spelling
	second, stderr := cliProcess(t, other, "serve", "--data-dir", abs)
	err = second.Run()
	ee, ok := err.(*osexec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("second serve: err = %v, want exit 3\n%s", err, stderr)
	}
	if want := "already running (pid " + strconv.Itoa(first.Process.Pid) + ")"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to say %q", stderr, want)
	}
	if !answers(abs) {
		t.Error("the first serve stopped answering on its socket after a second one tried to start")
	}
}

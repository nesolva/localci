package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/config"
)

// A data directory phase 1 made 0755, with a 0644 database and token, is private once `serve` has started, and
// the API answers on its socket.
func TestStartTightensAnOldDataDirAndServesTheSocket(t *testing.T) {
	data, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(data) })
	os.Chmod(data, 0o755)
	os.MkdirAll(filepath.Join(data, "logs"), 0o755)
	os.WriteFile(filepath.Join(data, "token"), []byte("old\n"), 0o644)
	os.WriteFile(filepath.Join(data, "localci.db"), nil, 0o644)

	m := capacity.Machine{CPUs: 2}
	eff, err := config.Load(config.Options{Machine: m, Env: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	ready, done := make(chan string, 1), make(chan error, 1)
	go func() {
		done <- Start(ctx, Options{Config: eff, DataDir: data, Machine: m, Port: -1, WorkerPort: -1, Log: log.New(io.Discard, "", 0),
			Ready: func(a string) { ready <- a }})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Start: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not listen")
	}
	for _, p := range []string{"", "logs"} {
		if got := mode(t, filepath.Join(data, p)); got != 0o700 {
			t.Errorf("%q is %o, want 700", p, got)
		}
	}
	for _, f := range []string{"token", "localci.db", "localci.db-wal", "localci.db-shm", SocketName} {
		if got := mode(t, filepath.Join(data, f)); got != 0o600 {
			t.Errorf("%s is %o, want 600", f, got)
		}
	}
	sock, err := FindSocket(data)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := socketClient(sock).Get(sockURL + "/api/runs")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("GET over the socket: %v %v", resp, err)
	}
	stop()
	if err := <-done; err != nil {
		t.Errorf("Start returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, SocketName)); err == nil {
		t.Error("the socket file outlived the server")
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

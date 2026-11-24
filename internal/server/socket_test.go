package server

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDir is a data directory whose socket path fits the platform limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestSocketIsPrivate(t *testing.T) {
	dir := shortDir(t)
	l, path, err := ListenSocket(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if path != filepath.Join(dir, SocketName) {
		t.Errorf("socket at %s, want it in the data directory", path)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %o, want 600", fi.Mode().Perm())
	}
	if got, err := FindSocket(dir); err != nil || got != path {
		t.Errorf("FindSocket = %q, %v", got, err)
	}
}

// A data directory path too long for the socket falls back to a private runtime directory, and the client still
// finds the socket through the path left in the data directory.
func TestSocketPathOverThePlatformLimitFallsBackToTheRuntimeDir(t *testing.T) {
	long := filepath.Join(shortDir(t), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	if fitsSocket(filepath.Join(long, SocketName)) {
		t.Fatal("the test path is meant to be too long")
	}
	t.Setenv("XDG_RUNTIME_DIR", shortDir(t))
	l, path, err := ListenSocket(long)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	defer os.RemoveAll(filepath.Dir(path))
	if filepath.Dir(path) == long {
		t.Fatal("socket stayed in the long data directory")
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("runtime directory: %v %v, want mode 700", fi, err)
	}
	got, err := FindSocket(long)
	if err != nil || got != path {
		t.Fatalf("FindSocket = %q, %v; want %q", got, err, path)
	}
	go func() {
		c, _ := l.Accept()
		if c != nil {
			c.Close()
		}
	}()
	c, err := net.Dial("unix", got)
	if err != nil {
		t.Fatalf("the path FindSocket returned does not connect: %v", err)
	}
	c.Close()
}

func TestStaleSocketIsReplacedOnlyWhenNothingAccepts(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, SocketName)
	// a server that died leaves its socket file behind
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("setup: no stale socket file")
	}
	l, _, err := ListenSocket(dir)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	defer l.Close()

	if _, _, err := ListenSocket(dir); err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Errorf("a live socket was replaced: %v", err)
	}
}

func TestSocketPathThatIsNotASocketIsNeverRemoved(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, SocketName)
	os.WriteFile(path, []byte("mine"), 0o600)
	if _, _, err := ListenSocket(dir); err == nil {
		t.Fatal("listened over a regular file")
	}
	if b, _ := os.ReadFile(path); string(b) != "mine" {
		t.Error("the file was removed")
	}
}

// writePointer plays a server that left its socket path in the data directory.
func writePointer(t *testing.T, dataDir, sockPath string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, pointerName), []byte(sockPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A data directory whose socket path is too long for the platform, so the socket path comes from the pointer file.
func longDataDir(t *testing.T) string {
	t.Helper()
	long := filepath.Join(shortDir(t), strings.Repeat("d", 60), strings.Repeat("e", 60))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	return long
}

// Another local user can create the predictable fallback directory first and wait for the CLI to dial into it with
// the API token. A directory that is not ours, not private or a symlink must never be handed to a client.
func TestClientIsNotPointedAtASocketDirectoryThatIsNotPrivate(t *testing.T) {
	for name, make := range map[string]func(t *testing.T, dir string){
		"loose mode": func(t *testing.T, dir string) {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil { // umask must not hide the bug
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, dir string) {
			real := shortDir(t)
			if err := os.Chmod(real, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, dir); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := longDataDir(t)
			dir := filepath.Join(shortDir(t), "rt")
			make(t, dir)
			writePointer(t, data, filepath.Join(dir, SocketName))
			if p, err := FindSocket(data); err == nil {
				t.Fatalf("FindSocket handed out %q from a directory that is not a private one of ours", p)
			}
		})
	}
}

func TestListenSocketRefusesAFallbackDirectoryThatIsNotPrivate(t *testing.T) {
	data := longDataDir(t)
	base := shortDir(t)
	t.Setenv("XDG_RUNTIME_DIR", base)
	// somebody made the predictable directory first, and loose
	dir, err := runtimeDirPath(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if l, _, err := ListenSocket(data); err == nil {
		l.Close()
		t.Fatal("served into a directory that is not private")
	}
}

func TestPointerFileIsRemovedWhenTheServerStops(t *testing.T) {
	data := longDataDir(t)
	t.Setenv("XDG_RUNTIME_DIR", shortDir(t))
	l, path, err := ListenSocket(data)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(path))
	l.Close()
	RemovePointer(data)
	if _, err := FindSocket(data); err == nil {
		t.Error("FindSocket still finds a socket after the server stopped")
	}
}

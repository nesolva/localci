package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// SocketName is the API socket's file name in the data directory.
const SocketName = "api.sock"

// pointerName holds the socket's path when it could not live in the data directory.
const pointerName = "api.sock.path"

// maxSocketPath is how long a Unix socket path may be (the platform's sun_path, with its terminating NUL).
func maxSocketPath() int {
	if runtime.GOOS == "linux" || runtime.GOOS == "android" {
		return 108
	}
	return 104
}

func fitsSocket(p string) bool { return len(p) < maxSocketPath() }

// FindSocket is the path a client should dial for the server of dataDir: the socket in the data directory, or the
// path the server left there when that was too long for the platform. It does not check that anything listens.
func FindSocket(dataDir string) (string, error) {
	p := filepath.Join(dataDir, SocketName)
	if fitsSocket(p) {
		return p, nil
	}
	b, err := os.ReadFile(filepath.Join(dataDir, pointerName))
	if err != nil {
		return "", errors.New("no server socket recorded in " + dataDir)
	}
	sock := strings.TrimSpace(string(b))
	if err := checkSocketDir(filepath.Dir(sock)); err != nil {
		return "", err
	}
	return sock, nil
}

// checkSocketDir refuses a socket directory that somebody else could have made or can write to: it must be a real
// directory (not a symlink), owned by this user, mode 0700. The fallback directory has a predictable name, so
// another local user can create it first and wait for a client to dial in with the API token.
func checkSocketDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the socket directory %s: %w", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing the socket directory %s: it is not a directory of yours (remove it and restart localci serve)", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("refusing the socket directory %s: mode is %o, it must be 700 (remove it and restart localci serve)", dir, fi.Mode().Perm())
	}
	return nil
}

// ListenSocket makes the Unix socket of dataDir and listens on it, mode 0600, in a 0700 directory. A socket file
// left by a server that died is replaced; one something still accepts on is an error.
func ListenSocket(dataDir string) (net.Listener, string, error) {
	path := filepath.Join(dataDir, SocketName)
	pointer := filepath.Join(dataDir, pointerName)
	if fitsSocket(path) {
		os.Remove(pointer)
	} else {
		dir, err := runtimeDir(dataDir)
		if err != nil {
			return nil, "", err
		}
		path = filepath.Join(dir, SocketName)
		if !fitsSocket(path) {
			return nil, "", fmt.Errorf("the socket path %s is too long for this platform; set TMPDIR or XDG_RUNTIME_DIR to a shorter directory", path)
		}
		if err := os.WriteFile(pointer, []byte(path+"\n"), 0o600); err != nil {
			return nil, "", err
		}
	}
	if err := clearStale(path); err != nil {
		return nil, "", err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, "", err
	}
	return l, path, nil
}

// clearStale removes a socket file nothing accepts on. Anything else at the path is left alone.
func clearStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return fmt.Errorf("another localci serve is already listening on %s", path)
	}
	return os.Remove(path)
}

// runtimeDir is the private directory for a socket that does not fit in the data directory: under
// $XDG_RUNTIME_DIR, else the per-user temporary directory. One per data directory.
func runtimeDir(dataDir string) (string, error) {
	dir, err := runtimeDirPath(dataDir)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := checkSocketDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// runtimeDirPath is where the fallback directory of dataDir lives. The name is predictable, so whoever uses the
// directory must check who owns it.
func runtimeDirPath(dataDir string) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" || runtime.GOOS == "darwin" {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(dataDir))
	return filepath.Join(base, fmt.Sprintf("localci-%d-%s", os.Getuid(), hex.EncodeToString(sum[:4]))), nil
}

// RemovePointer forgets where the socket was, once the server stops.
func RemovePointer(dataDir string) { os.Remove(filepath.Join(dataDir, pointerName)) }

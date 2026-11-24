package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// What the data directory holds is private to the user running localci: directories 0700, files 0600.
const (
	DirMode  fs.FileMode = 0o700
	FileMode fs.FileMode = 0o600
)

// Loose is a path whose mode lets other users in.
type Loose struct {
	Path string
	Mode fs.FileMode
	Want fs.FileMode
}

// privatePaths lists what exists of the data directory's own paths that need tightening: the directory itself,
// the logs and checkpoint directories, the database with its WAL and shared-memory files, the token and the worker listener's certificate and key.
func privatePaths(dataDir string) (dirs, files []string) {
	dirs = []string{dataDir, filepath.Join(dataDir, "logs")}
	if kids, err := os.ReadDir(filepath.Join(dataDir, "checkpoints")); err == nil {
		dirs = append(dirs, filepath.Join(dataDir, "checkpoints"))
		for _, k := range kids {
			if k.IsDir() {
				dirs = append(dirs, filepath.Join(dataDir, "checkpoints", k.Name()))
			}
		}
	}
	db := filepath.Join(dataDir, "localci.db")
	files = []string{db, db + "-wal", db + "-shm", filepath.Join(dataDir, "token"),
		filepath.Join(dataDir, "worker-cert.pem"), filepath.Join(dataDir, "worker-key.pem")}
	return dirs, files
}

// LooseModes reports every existing private path whose mode gives group or others any access.
func LooseModes(dataDir string) ([]Loose, error) {
	dirs, files := privatePaths(dataDir)
	var out []Loose
	check := func(p string, want fs.FileMode) error {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if m := fi.Mode().Perm(); m&^want != 0 {
			out = append(out, Loose{p, m, want})
		}
		return nil
	}
	for _, d := range dirs {
		if err := check(d, DirMode); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		if err := check(f, FileMode); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Harden creates the data directory 0700 and tightens what an earlier version left looser. Call it before
// anything else is written there.
func Harden(dataDir string) error {
	if err := os.MkdirAll(dataDir, DirMode); err != nil {
		return err
	}
	loose, err := LooseModes(dataDir)
	if err != nil {
		return err
	}
	for _, l := range loose {
		if err := os.Chmod(l.Path, l.Want); err != nil {
			return fmt.Errorf("tighten %s: %w", l.Path, err)
		}
	}
	return nil
}

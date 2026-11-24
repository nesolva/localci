package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// payload is what a checkpoint keeps on disk: the browser storage state (session cookies, so it never leaves the
// data directory and never goes into an event) and the flow's outputs.
type payload struct {
	StorageState json.RawMessage `json:"storageState,omitempty"`
	Outputs      json.RawMessage `json:"outputs,omitempty"`
}

// dir is where a suite's checkpoint files live, created 0700 (the files hold session cookies).
func (s *Service) dir(suite string) (string, error) {
	if !safeSuite(suite) {
		return "", fmt.Errorf("suite name %q cannot name a checkpoint directory", suite)
	}
	base := filepath.Join(s.DataDir, "checkpoints")
	dir := filepath.Join(base, suite)
	for _, d := range []string{base, dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(d, 0o700); err != nil { // an older, looser directory is tightened
			return "", err
		}
	}
	return dir, nil
}

func (s *Service) file(suite, key string) (string, error) {
	dir, err := s.dir(suite)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}

// writeFile stores p atomically: a kill leaves either no file or the whole one.
func writeFile(path string, p payload) (int64, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		tmp.Close()
		return 0, err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return int64(len(b)), os.Rename(tmp.Name(), path)
}

func readFile(path string) (payload, error) {
	var p payload
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("checkpoint file %s: %w", filepath.Base(path), err)
	}
	return p, nil
}

func removeFile(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

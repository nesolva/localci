package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is where the write token lives under the data directory.
func TokenFile(dataDir string) string { return filepath.Join(dataDir, "token") }

// LoadToken returns the token in the data directory, creating it (mode 0600) on first use. Anything that can read
// the file may start and cancel runs; that is the same trust as running the CLI.
func LoadToken(dataDir string) (string, error) {
	path := TokenFile(dataDir)
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken reads the token a running server uses; it never creates one.
func ReadToken(dataDir string) (string, error) {
	b, err := os.ReadFile(TokenFile(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("no token yet: start `localci serve` once")
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func tokenMatches(want, got string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

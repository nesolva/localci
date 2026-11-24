package shard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// instanceFile holds the instance id in the data directory.
const instanceFile = "instance-id"

// InstanceID is the stable id of the localci that owns dataDir, made on first use and kept in the data directory. It
// goes into the names of the databases this localci makes on a Postgres server, so a second localci on the same
// server (another data directory) never gets the same names and its sweep never matches them. When the id cannot be
// stored, it is derived from the data directory's path, which is stable too.
func InstanceID(dataDir string) string {
	path := filepath.Join(dataDir, instanceFile)
	read := func() string {
		b, err := os.ReadFile(path)
		if id := strings.TrimSpace(string(b)); err == nil && id != "" {
			return id
		}
		return ""
	}
	if id := read(); id != "" {
		return id
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err == nil {
		id := hex.EncodeToString(buf)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.WriteString(id + "\n")
			if cerr := f.Close(); werr == nil && cerr == nil {
				return id
			}
			os.Remove(path)
		} else if errors.Is(err, fs.ErrExist) {
			// another process made it a moment ago
			if id := read(); id != "" {
				return id
			}
		}
	}
	sum := sha256.Sum256([]byte(dataDir))
	return hex.EncodeToString(sum[:8])
}

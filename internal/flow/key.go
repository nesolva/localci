package flow

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// Key identifies a checkpoint: a hash of the suite, the flow's name, its source, its inputs, the commits under test,
// the database template and the key of whatever ran before it in the test. Any of them changing gives a new key, so
// a checkpoint never crosses suites, commits, templates or edits of the flow (the suite matters because the
// snapshot database is named by the key alone). source and inputs are hashes the test package made; the rest the
// core knows.
func Key(suite, name, source, inputs, build, template, parent string) string {
	h := sha256.New()
	for _, part := range []string{suite, name, source, inputs, build, template, parent} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var keyRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidKey reports whether k is a key Key made. Keys come in over HTTP and become file and database names.
func ValidKey(k string) bool { return keyRE.MatchString(k) }

// CheckpointDB is the name of the database that holds the snapshot of key: localci_ck_<key16>.
func CheckpointDB(key string) string { return "localci_ck_" + key[:16] }

var suiteRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func safeSuite(name string) bool { return suiteRE.MatchString(name) }

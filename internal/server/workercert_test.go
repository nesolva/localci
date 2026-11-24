package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerCertIsGeneratedOnceAndLoggedWithItsFingerprint(t *testing.T) {
	dir := t.TempDir()
	var logged []string
	logf := func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	_, fp, err := LoadWorkerCert(dir, logf)
	if err != nil {
		t.Fatal(err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], fp) {
		t.Fatalf("generation log = %q, want one line with the fingerprint", logged)
	}
	if _, fp2, err := LoadWorkerCert(dir, logf); err != nil || fp2 != fp || len(logged) != 1 {
		t.Fatalf("reload: fp %s vs %s, err %v, log %q", fp2, fp, err, logged)
	}
}

// A pair that is there but unusable is never replaced: that would break every pinned worker without a word.
func TestWorkerCertNeverReplacesAPairItCannotUse(t *testing.T) {
	good := t.TempDir()
	if _, _, err := LoadWorkerCert(good, nil); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if _, _, err := LoadWorkerCert(other, nil); err != nil {
		t.Fatal(err)
	}
	read := func(dir, f string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		name  string
		setup func(dir string)
	}{
		{"garbage certificate", func(dir string) { os.WriteFile(filepath.Join(dir, WorkerCertFile), []byte("junk"), 0o600) }},
		{"missing key", func(dir string) { os.Remove(filepath.Join(dir, WorkerKeyFile)) }},
		{"missing certificate", func(dir string) { os.Remove(filepath.Join(dir, WorkerCertFile)) }},
		{"mismatched pair", func(dir string) {
			os.WriteFile(filepath.Join(dir, WorkerKeyFile), read(other, WorkerKeyFile), 0o600)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{WorkerCertFile, WorkerKeyFile} {
				os.WriteFile(filepath.Join(dir, f), read(good, f), 0o600)
			}
			c.setup(dir)
			before := map[string][]byte{}
			for _, f := range []string{WorkerCertFile, WorkerKeyFile} {
				before[f], _ = os.ReadFile(filepath.Join(dir, f))
			}
			var logged []string
			_, _, err := LoadWorkerCert(dir, func(f string, a ...any) { logged = append(logged, f) })
			if err == nil || !strings.Contains(err.Error(), WorkerCertFile) || !strings.Contains(err.Error(), WorkerKeyFile) {
				t.Fatalf("err = %v, want one naming both files", err)
			}
			if len(logged) != 0 {
				t.Fatalf("generated a pair: %q", logged)
			}
			for f, b := range before {
				if got, _ := os.ReadFile(filepath.Join(dir, f)); string(got) != string(b) {
					t.Errorf("%s was rewritten", f)
				}
			}
		})
	}
}

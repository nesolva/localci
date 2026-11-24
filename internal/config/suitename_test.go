package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSuiteNamesMustBeContextSafe(t *testing.T) {
	for name, ok := range map[string]bool{"e2e": true, "checkout-flow": true, "checks": false, "a/b": false, "E2E": false, "my suite": false, "a b (affected)": false, "": false} {
		if err := CheckSuiteName(name); (err == nil) != ok {
			t.Errorf("CheckSuiteName(%q) = %v, want ok=%v", name, err, ok)
		}
	}
}

// A suite named checks or holding a slash would post a status context that collides with a check's.
func TestConfigFileWithABadSuiteNameIsAnErrorAtItsLine(t *testing.T) {
	for _, name := range []string{"checks", `"a/b"`} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		os.WriteFile(path, []byte("suites:\n  e2e: {}\n  "+name+": {}\n"), 0o644)
		_, err := Load(Options{Path: path, Machine: testMachine, Env: func(string) string { return "" }})
		if err == nil || !strings.Contains(err.Error(), ":3") {
			t.Errorf("suite %s: err = %v, want a config error pointing at line 3", name, err)
		}
	}
}

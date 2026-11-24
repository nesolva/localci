package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitsCarryTheEnvironmentThatDecidesWhereDataLives(t *testing.T) {
	// The LOCALCI_* variables of a run (a shell inside a suite step) are not configuration: LOCALCI_DB_URL may hold
	// a database password and must never land in the plain-text unit file.
	environ := []string{"HOME=/home/a", "GITHUB_TOKEN=ghp_secret", "XDG_DATA_HOME=/data x", "XDG_CONFIG_HOME=/cfg",
		"LOCALCI_SERVER_PORT=9000", "LOCALCI_CONFIG=rel.yaml", "LOCALCI_EMPTY=x", "SSH_AUTH_SOCK=/tmp/agent",
		"LOCALCI_DB_URL=postgres://u:dbsecret@h/db", "LOCALCI_API=http://127.0.0.1:1", "LOCALCI_RUN=r1", "LOCALCI_SHARD=2"}
	s := spec
	s.Env = CarriedEnv(environ)
	for name, body := range map[string]string{"plist": Plist(s), "unit": UnitFile(s)} {
		for _, want := range []string{"XDG_DATA_HOME", "/data x", "XDG_CONFIG_HOME", "LOCALCI_SERVER_PORT", "PATH"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s misses %q:\n%s", name, want, body)
			}
		}
		for _, bad := range []string{"GITHUB_TOKEN", "ghp_secret", "SSH_AUTH_SOCK", "LOCALCI_CONFIG", "LOCALCI_EMPTY",
			"LOCALCI_DB_URL", "dbsecret", "LOCALCI_API", "LOCALCI_RUN", "LOCALCI_SHARD"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s must not carry %q:\n%s", name, bad, body)
			}
		}
	}
}

// systemd reads a unit line by line: a newline inside a value would start a new directive.
func TestInstallRefusesNewlinesInTheUnit(t *testing.T) {
	var calls []call
	m := Manager{GOOS: "linux", Home: t.TempDir(), Dir: t.TempDir(), Run: recorder(&calls)}
	s := spec
	s.Env = []string{"XDG_DATA_HOME=/data\nExecStartPre=/bin/evil"}
	if _, err := m.Install(s, false); err == nil {
		t.Fatal("a value with a newline must be refused")
	}
	if entries, _ := os.ReadDir(m.Dir); len(entries) != 0 {
		t.Fatalf("nothing must be written, found %v", entries[0].Name())
	}
	s = spec
	s.Args = append([]string{}, "serve", "--config", filepath.Join("/a\nb", "c.yaml"))
	if _, err := m.Install(s, false); err == nil {
		t.Fatal("an argument with a newline must be refused")
	}
}

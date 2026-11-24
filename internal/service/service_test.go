package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// spec has the awkward characters real machines have: spaces in paths and a PATH with several entries.
var spec = Spec{
	Binary: "/opt/my tools/localci",
	Args:   []string{"serve", "--config", "/home/a b/config.yaml"},
	Path:   "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin",
}

type call struct{ name, args string }

func recorder(calls *[]call) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, call{name, strings.Join(args, " ")})
		return nil, nil
	}
}

func TestInstalledPlistIsValid(t *testing.T) {
	var calls []call
	m := Manager{GOOS: "darwin", Home: t.TempDir(), Dir: t.TempDir(), UID: 501, Run: recorder(&calls)}
	s := spec
	s.LogDir = t.TempDir()
	r, err := m.Install(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 || r.Started {
		t.Errorf("start=false must not touch launchd: %v", calls)
	}
	if r.File != filepath.Join(m.Dir, Label+".plist") || r.Log != filepath.Join(s.LogDir, "serve.log") {
		t.Errorf("file %q log %q", r.File, r.Log)
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	if out, err := exec.Command(plutil, "-lint", r.File).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	body, _ := os.ReadFile(r.File)
	for _, want := range []string{"/opt/my tools/localci", "/home/a b/config.yaml", s.Path} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plist lacks %q", want)
		}
	}
}

func TestPlistEscapesXMLInArguments(t *testing.T) {
	s := spec
	s.Args = []string{"serve", "--token", "a&b<c>"}
	p := filepath.Join(t.TempDir(), "x.plist")
	if err := os.WriteFile(p, []byte(Plist(s)), 0o644); err != nil {
		t.Fatal(err)
	}
	if plutil, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint: %v: %s", err, out)
		}
	}
	if strings.Contains(Plist(s), "a&b<c>") {
		t.Error("raw & and < in a plist make it invalid")
	}
}

func TestInstalledUnitIsValid(t *testing.T) {
	var calls []call
	m := Manager{GOOS: "linux", Home: t.TempDir(), Dir: t.TempDir(), Run: recorder(&calls)}
	r, err := m.Install(spec, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Errorf("start=false must not touch systemd: %v", calls)
	}
	body, _ := os.ReadFile(r.File)
	if !strings.Contains(string(body), `ExecStart="/opt/my tools/localci" serve --config "/home/a b/config.yaml"`) {
		t.Errorf("ExecStart must quote words with spaces:\n%s", body)
	}
	if !strings.Contains(string(body), "Environment=PATH=") && !strings.Contains(string(body), `Environment="PATH=`) {
		t.Errorf("the unit must carry the PATH:\n%s", body)
	}
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not available")
	}
	// A unit whose binary does not exist makes verify complain about that alone; syntax errors say more.
	out, _ := exec.Command(analyze, "verify", "--user", r.File).CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, r.File) && !strings.Contains(line, "Executable") && !strings.Contains(line, "not executable") {
			t.Errorf("systemd-analyze verify: %s", line)
		}
	}
}

func TestStartLoadsThenKickstartsOnMac(t *testing.T) {
	var calls []call
	m := Manager{GOOS: "darwin", Home: t.TempDir(), Dir: t.TempDir(), UID: 501, Run: recorder(&calls)}
	s := spec
	s.LogDir = t.TempDir()
	r, err := m.Install(s, true)
	if err != nil || !r.Started {
		t.Fatalf("Install: %+v %v", r, err)
	}
	var got []string
	for _, c := range calls {
		got = append(got, c.name+" "+c.args)
	}
	want := []string{
		"launchctl bootout gui/501/" + Label,
		"launchctl bootstrap gui/501 " + r.File,
		"launchctl kickstart -k gui/501/" + Label,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestUninstallRemovesTheFileAndSaysWhenThereIsNone(t *testing.T) {
	var calls []call
	m := Manager{GOOS: "darwin", Home: t.TempDir(), Dir: t.TempDir(), UID: 501, Run: recorder(&calls)}
	s := spec
	s.LogDir = t.TempDir()
	r, _ := m.Install(s, false)
	if _, err := m.Uninstall(true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.File); !os.IsNotExist(err) {
		t.Errorf("the plist must be gone: %v", err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0].args, "bootout") {
		t.Errorf("uninstall must stop the agent first: %v", calls)
	}
	if _, err := m.Uninstall(true); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("second uninstall: %v", err)
	}
}

func TestUnsupportedSystemIsRefused(t *testing.T) {
	m := Manager{GOOS: "windows", Home: t.TempDir()}
	if _, err := m.Install(spec, false); err == nil {
		t.Error("expected an error")
	}
}

// Package service installs `localci serve` as a background service for the current user: a launchd agent on macOS,
// a systemd --user unit on Linux. It writes the unit file, and starts it unless told not to.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nesolva/localci/internal/config"
)

// Label names the launchd agent; Unit is the systemd unit's file name.
const (
	Label    = "io.localci.serve"
	UnitName = "localci.service"
)

// Spec is what the service runs.
type Spec struct {
	Binary string   // absolute path of the localci binary
	Args   []string // its arguments, "serve" first
	Path   string   // the PATH the service gets: services start with a bare one, and checks need the user's tools
	Env    []string // more KEY=VALUE the service gets: see CarriedEnv
	LogDir string   // macOS: where the service's output goes
}

func (s Spec) environment() []string {
	var out []string
	if s.Path != "" {
		out = append(out, "PATH="+s.Path)
	}
	return append(out, s.Env...)
}

// CarriedEnv picks, from an environment (os.Environ form), what the service must see to behave like the CLI that
// installed it: where the CLI keeps its data and config (XDG_DATA_HOME, XDG_CONFIG_HOME) and the LOCALCI_*
// variables that override a configuration key. LOCALCI_CONFIG is left out because the install writes the resolved
// path as --config. Nothing else is carried: never a token, nor the LOCALCI_* variables a run gives its steps
// (LOCALCI_DB_URL may hold a password), which would end up in a plain-text file.
func CarriedEnv(environ []string) []string {
	carried := map[string]bool{"XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true}
	for _, name := range config.EnvNames() {
		carried[name] = true
	}
	var out []string
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && v != "" && carried[k] {
			out = append(out, kv)
		}
	}
	sort.Strings(out)
	return out
}

// checkLines refuses a spec with a line break in a value: a unit file is read line by line, so the rest of the value
// would become a directive of its own.
func (s Spec) checkLines() error {
	for _, v := range append(append([]string{s.Binary, s.Path, s.LogDir}, s.Args...), s.Env...) {
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("service: %q contains a line break", v)
		}
	}
	return nil
}

// Result says where things ended up.
type Result struct {
	File    string // the unit file
	Log     string // where to read the service's output
	Started bool
}

// Manager installs a Spec. The zero value is not usable: set GOOS, Home and Run (see System).
type Manager struct {
	GOOS string
	Home string
	// Dir is where the unit file goes; empty means the OS's per-user location.
	Dir string
	UID int
	// Run executes a command and returns its combined output; tests replace it.
	Run func(name string, args ...string) ([]byte, error)
}

// System is a Manager for this machine.
func System(goos string) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	return Manager{GOOS: goos, Home: home, UID: os.Getuid(), Run: func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}}, nil
}

// DefaultLogDir is where the macOS agent writes its output.
func DefaultLogDir(home string) string { return filepath.Join(home, "Library", "Logs", "localci") }

func (m Manager) file() (string, error) {
	switch m.GOOS {
	case "darwin":
		dir := m.Dir
		if dir == "" {
			dir = filepath.Join(m.Home, "Library", "LaunchAgents")
		}
		return filepath.Join(dir, Label+".plist"), nil
	case "linux":
		dir := m.Dir
		if dir == "" {
			dir = filepath.Join(m.Home, ".config", "systemd", "user")
		}
		return filepath.Join(dir, UnitName), nil
	}
	return "", fmt.Errorf("services are supported on macOS (launchd) and Linux (systemd --user), not %s", m.GOOS)
}

func (m Manager) domain() string { return fmt.Sprintf("gui/%d", m.UID) }

// Install writes the unit file and, when start is set, loads it and starts it.
func (m Manager) Install(s Spec, start bool) (Result, error) {
	var r Result
	file, err := m.file()
	if err != nil {
		return r, err
	}
	if s.Binary == "" || len(s.Args) == 0 {
		return r, errors.New("service: a binary and its arguments are required")
	}
	if err := s.checkLines(); err != nil {
		return r, err
	}
	r.File = file
	var body string
	if m.GOOS == "darwin" {
		if s.LogDir == "" {
			s.LogDir = DefaultLogDir(m.Home)
		}
		if err := os.MkdirAll(s.LogDir, 0o755); err != nil {
			return r, err
		}
		r.Log = filepath.Join(s.LogDir, "serve.log")
		body = Plist(s)
	} else {
		r.Log = "journalctl --user -u " + UnitName
		body = UnitFile(s)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return r, err
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		return r, err
	}
	if !start {
		return r, nil
	}
	if m.GOOS == "darwin" {
		m.Run("launchctl", "bootout", m.domain()+"/"+Label) // a previous copy; not an error when there is none
		if out, err := m.Run("launchctl", "bootstrap", m.domain(), file); err != nil {
			return r, fmt.Errorf("launchctl bootstrap: %v: %s", err, bytes.TrimSpace(out))
		}
		if out, err := m.Run("launchctl", "kickstart", "-k", m.domain()+"/"+Label); err != nil {
			return r, fmt.Errorf("launchctl kickstart: %v: %s", err, bytes.TrimSpace(out))
		}
	} else {
		if out, err := m.Run("systemctl", "--user", "daemon-reload"); err != nil {
			return r, fmt.Errorf("systemctl daemon-reload: %v: %s", err, bytes.TrimSpace(out))
		}
		if out, err := m.Run("systemctl", "--user", "enable", "--now", UnitName); err != nil {
			return r, fmt.Errorf("systemctl enable: %v: %s", err, bytes.TrimSpace(out))
		}
	}
	r.Started = true
	return r, nil
}

// Uninstall stops the service (when stop is set) and removes its unit file.
func (m Manager) Uninstall(stop bool) (string, error) {
	file, err := m.file()
	if err != nil {
		return "", err
	}
	if stop {
		if m.GOOS == "darwin" {
			m.Run("launchctl", "bootout", m.domain()+"/"+Label)
		} else {
			m.Run("systemctl", "--user", "disable", "--now", UnitName)
		}
	}
	if err := os.Remove(file); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return file, errors.New("the service is not installed (no " + file + ")")
		}
		return file, err
	}
	if stop && m.GOOS == "linux" {
		m.Run("systemctl", "--user", "daemon-reload")
	}
	return file, nil
}

// Status says whether the unit exists and whether it is running.
func (m Manager) Status() (string, error) {
	file, err := m.file()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(file); err != nil {
		return "not installed", nil
	}
	if m.GOOS == "darwin" {
		out, err := m.Run("launchctl", "print", m.domain()+"/"+Label)
		switch {
		case err != nil:
			return "installed, not loaded (run `localci service install`)", nil
		case strings.Contains(string(out), "state = running"):
			return "running", nil
		}
		return "loaded, not running", nil
	}
	out, _ := m.Run("systemctl", "--user", "is-active", UnitName)
	return "installed, " + strings.TrimSpace(string(out)), nil
}

// Plist renders the launchd agent.
func Plist(s Spec) string {
	var b strings.Builder
	esc := func(v string) string {
		var x bytes.Buffer
		xml.EscapeText(&x, []byte(v))
		return x.String()
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{s.Binary}, s.Args...) {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	if vars := s.environment(); len(vars) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, kv := range vars {
			k, v, _ := strings.Cut(kv, "=")
			b.WriteString("\t\t<key>" + esc(k) + "</key>\n\t\t<string>" + esc(v) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<true/>\n\t<key>ProcessType</key>\n\t<string>Background</string>\n")
	if s.LogDir != "" {
		log := filepath.Join(s.LogDir, "serve.log")
		b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + esc(log) + "</string>\n")
		b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + esc(log) + "</string>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// UnitFile renders the systemd --user unit.
func UnitFile(s Spec) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=localci: e2e suites and pull request checks on this machine\nAfter=network-online.target\n\n[Service]\n")
	exec := make([]string, 0, len(s.Args)+1)
	for _, a := range append([]string{s.Binary}, s.Args...) {
		exec = append(exec, unitQuote(a))
	}
	b.WriteString("ExecStart=" + strings.Join(exec, " ") + "\n")
	for _, kv := range s.environment() {
		b.WriteString("Environment=" + unitQuote(kv) + "\n")
	}
	b.WriteString("Restart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

// unitQuote quotes a word the way systemd reads command lines and environment assignments.
func unitQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(s) + `"`
}

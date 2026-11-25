package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/capacity"
)

const gib = uint64(1) << 30

var testMachine = capacity.Machine{CPUs: 8, FreeMemory: 16 * gib, Docker: true, DockerMemory: 8 * gib, Mise: true}

func load(t *testing.T, o Options) *Effective {
	t.Helper()
	if o.Machine.CPUs == 0 {
		o.Machine = testMachine
	}
	if o.Env == nil {
		o.Env = func(string) string { return "" }
	}
	eff, err := Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return eff
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileEnvFlagLayering(t *testing.T) {
	file := writeFile(t, "repos: [{name: acme/api}]\nwatch:\n  poll: 2m\nserver:\n  port: 9000\n")
	env := func(k string) string {
		return map[string]string{"LOCALCI_WATCH_POLL": "5m", "LOCALCI_WATCH_DRAFTS": "true"}[k]
	}

	e := load(t, Options{Path: file})
	if e.Watch.Poll.String() != "2m0s" || e.Sources["watch.poll"] != SourceFile {
		t.Errorf("file layer: poll=%v source=%s", e.Watch.Poll, e.Sources["watch.poll"])
	}
	if e.Watch.MaxAge.Hours() != 24 || e.Sources["watch.max_age"] != SourceDefault {
		t.Errorf("untouched key must stay default: %v %s", e.Watch.MaxAge, e.Sources["watch.max_age"])
	}

	e = load(t, Options{Path: file, Env: env})
	if e.Watch.Poll.String() != "5m0s" || e.Sources["watch.poll"] != SourceEnv {
		t.Errorf("env must beat file: poll=%v source=%s", e.Watch.Poll, e.Sources["watch.poll"])
	}
	if !e.Watch.Drafts || e.Sources["watch.drafts"] != SourceEnv {
		t.Errorf("env-only key: drafts=%v source=%s", e.Watch.Drafts, e.Sources["watch.drafts"])
	}
	if e.Server.Port != 9000 || e.Sources["server.port"] != SourceFile {
		t.Errorf("file value must survive other layers: port=%d source=%s", e.Server.Port, e.Sources["server.port"])
	}

	e = load(t, Options{Path: file, Env: env, Flags: map[string]string{"watch.poll": "30s"}})
	if e.Watch.Poll.String() != "30s" || e.Sources["watch.poll"] != SourceFlag {
		t.Errorf("flag must beat env: poll=%v source=%s", e.Watch.Poll, e.Sources["watch.poll"])
	}
}

func TestMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := Load(Options{Path: missing, Machine: testMachine}); err == nil {
		t.Error("an explicitly named file that does not exist must be an error")
	}
	env := func(k string) string {
		if k == "LOCALCI_CONFIG" {
			return missing
		}
		return ""
	}
	if _, err := Load(Options{Env: env, Machine: testMachine}); err == nil {
		t.Error("LOCALCI_CONFIG pointing at a missing file must be an error")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	e := load(t, Options{})
	if e.FileFound {
		t.Error("a missing default file is fine and must load defaults")
	}
}

func TestAutoFollowsMachineAndOverrides(t *testing.T) {
	e := load(t, Options{Path: writeFile(t, "repos: [{name: acme/api}]\n")})
	if e.Capacity.CPUs != 8 || e.Sources["capacity.cpus"] != SourceAuto {
		t.Errorf("cpus: %d %s", e.Capacity.CPUs, e.Sources["capacity.cpus"])
	}
	// Docker's 8 GiB is the tightest limit: 8 / 2 = 4 slots; jobs are half.
	if e.Capacity.CheckSlots != 4 || e.Capacity.JobSlots != 2 || e.Sources["capacity.check_slots"] != SourceAuto {
		t.Errorf("slots: %d/%d %s", e.Capacity.CheckSlots, e.Capacity.JobSlots, e.Sources["capacity.check_slots"])
	}
	if e.Fidelity.Toolchains != "mise" || e.Sources["fidelity.toolchains"] != SourceAuto {
		t.Errorf("toolchains: %s %s", e.Fidelity.Toolchains, e.Sources["fidelity.toolchains"])
	}

	// A hand-set CPU budget lowers the slots that auto picks, and stays marked as from the file.
	e = load(t, Options{Path: writeFile(t, "capacity:\n  cpus: 2\n")})
	if e.Capacity.CheckSlots != 2 || e.Sources["capacity.cpus"] != SourceFile || e.Sources["capacity.check_slots"] != SourceAuto {
		t.Errorf("override: slots=%d cpus-source=%s slots-source=%s", e.Capacity.CheckSlots, e.Sources["capacity.cpus"], e.Sources["capacity.check_slots"])
	}

	// No Docker and no mise: memory and CPUs decide, toolchains fall back to containers.
	m := capacity.Machine{CPUs: 16, FreeMemory: 6 * gib}
	e = load(t, Options{Machine: m, Path: writeFile(t, "")})
	if e.Capacity.CheckSlots != 4 || e.Fidelity.Toolchains != "containers" || e.Capacity.DockerMemory != 0 {
		t.Errorf("no docker/mise: slots=%d toolchains=%s", e.Capacity.CheckSlots, e.Fidelity.Toolchains)
	}

	// Explicit slots win over auto, and an explicit "auto" is still auto.
	e = load(t, Options{Path: writeFile(t, "capacity:\n  check_slots: 6\n  job_slots: auto\n")})
	if e.Capacity.CheckSlots != 6 || e.Sources["capacity.check_slots"] != SourceFile || e.Capacity.JobSlots != 3 {
		t.Errorf("explicit slots: %d/%d", e.Capacity.CheckSlots, e.Capacity.JobSlots)
	}
}

func TestReposAndSizesFromFile(t *testing.T) {
	e := load(t, Options{Path: filepath.Join("testdata", "good.yaml")})
	if len(e.Repos) != 2 || e.Repos[0].Name != "acme/api" || e.Repos[0].SkipWorkflows[0] != "release.yml" {
		t.Errorf("repos: %+v", e.Repos)
	}
	if e.Capacity.Memory != 8*gib || e.Server.Port != 9000 {
		t.Errorf("memory=%d port=%d", e.Capacity.Memory, e.Server.Port)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if err := load(t, Options{Path: writeFile(t, "")}).Validate(); err == nil {
		t.Error("a config without repos must fail Validate, though it still loads for inspection")
	}
}

func TestInvalidConfigNamesFileLineAndField(t *testing.T) {
	cases := []struct {
		fixture string
		want    string // "<file>:<line>: <field>: <message prefix>"
	}{
		{"bad_duration.yaml", "bad_duration.yaml:5: watch.poll: "},
		{"unknown_key.yaml", `unknown_key.yaml:4: watch.pol: unknown setting (did you mean "poll"?)`},
		{"bad_enum.yaml", "bad_enum.yaml:4: watch.forks: "},
		{"bad_repo.yaml", "bad_repo.yaml:3: repos[1].name: "},
		{"bad_int.yaml", "bad_int.yaml:4: capacity.check_slots: "},
		{"bad_bool.yaml", "bad_bool.yaml:4: watch.drafts: "},
		{"bad_section.yaml", "bad_section.yaml:3: watch: expected a mapping"},
		{"bad_skip.yaml", "bad_skip.yaml:4: repos[0].workflows.skip: expected a list"},
		{"bad_yaml.yaml", "bad_yaml.yaml:"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, err := Load(Options{Path: filepath.Join("testdata", tc.fixture), Machine: testMachine,
				Env: func(string) string { return "" }})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := Load(Options{Path: writeFile(t, "watch:\n  poll: x\n  forks: y\nserver:\n  port: 0\n"), Machine: testMachine,
		Env: func(string) string { return "" }})
	es, ok := err.(Errors)
	if !ok || len(es) != 3 {
		t.Fatalf("want 3 errors, got %v", err)
	}
}

func TestBadEnvAndFlagNameTheirSource(t *testing.T) {
	_, err := Load(Options{Machine: testMachine, Path: writeFile(t, ""),
		Env: func(k string) string {
			if k == "LOCALCI_SERVER_PORT" {
				return "99999"
			}
			return ""
		},
		Flags: map[string]string{"watch.forks": "maybe"}})
	if err == nil || !strings.Contains(err.Error(), "env LOCALCI_SERVER_PORT") || !strings.Contains(err.Error(), "flag --watch-forks") {
		t.Errorf("got %v", err)
	}
}

func TestJSONHasValueAndSourceForEveryKey(t *testing.T) {
	e := load(t, Options{Path: filepath.Join("testdata", "good.yaml")})
	b, err := e.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	slots := raw["capacity"].(map[string]any)["check_slots"].(map[string]any)
	if slots["source"] != "auto" || slots["value"].(float64) < 1 {
		t.Errorf("capacity.check_slots: %v", slots)
	}
	for _, f := range Fields {
		node := raw
		parts := strings.Split(f.Key, ".")
		for _, p := range parts[:len(parts)-1] {
			node = node[p].(map[string]any)
		}
		leaf, ok := node[parts[len(parts)-1]].(map[string]any)
		if !ok || leaf["source"] == nil {
			t.Errorf("%s: missing value/source in JSON", f.Key)
		}
	}
	if raw["repos"].(map[string]any)["source"] != "file" {
		t.Errorf("repos source: %v", raw["repos"])
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{
		"512MiB": 512 << 20, "8GiB": 8 << 30, "6g": 6 << 30, "1.5G": 3 << 29, "1024": 1024, "0": 0, " 2 GB ": 2 << 30,
	} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "lots", "5 parsecs", "-1GiB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

// A stale docs/config.md means someone changed the schema without regenerating.
func TestDocsAreUpToDate(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", "docs", "config.md"))
	if err != nil {
		t.Fatalf("docs/config.md missing: %v (run `go generate ./...`)", err)
	}
	if string(got) != Markdown() {
		t.Error("docs/config.md is stale: run `go generate ./...` and commit the result")
	}
}

func TestReposRunChecksUnlessTurnedOff(t *testing.T) {
	e := load(t, Options{Path: writeFile(t, "repos:\n  - name: acme/api\n  - name: acme/web\n    checks: false\n")})
	if len(e.Repos) != 2 || !e.Repos[0].Checks || e.Repos[1].Checks {
		t.Errorf("checks must default to true and honour false: %+v", e.Repos)
	}
}

func writeConfig(t *testing.T, body string) Options {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{Path: p, Machine: testMachine, Env: func(string) string { return "" }}
}

func TestRepoPathIsExpandedAndResolved(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := filepath.Join(home, "code", "local")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)

	for _, p := range []string{"~/code/local", link} {
		e := load(t, writeConfig(t, "repos:\n  - name: acme/local\n    path: "+p+"\n    checks: false\n"))
		if len(e.Repos) != 1 || e.Repos[0].Path != want || e.Repos[0].Checks {
			t.Errorf("path %q: repos = %+v, want path %s", p, e.Repos, want)
		}
	}
	// An entry without a path stays as it was.
	e := load(t, writeConfig(t, "repos:\n  - name: acme/api\n"))
	if e.Repos[0].Path != "" || !e.Repos[0].Checks {
		t.Errorf("repos = %+v", e.Repos)
	}
}

func TestRepoPathMustBeAnExistingDirectoryAndUnique(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o644)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ yaml, want string }{
		"missing": {"repos:\n  - name: acme/local\n    path: " + filepath.Join(dir, "nope") + "\n",
			"config.yaml:3: repos[0].path: acme/local: "},
		"a file":   {"repos:\n  - name: acme/local\n    path: " + file + "\n", "repos[0].path: acme/local: " + file + " is not a directory"},
		"relative": {"repos:\n  - name: acme/local\n    path: code/local\n", "repos[0].path: acme/local: "},
		"empty":    {"repos:\n  - name: acme/local\n    path: \"\"\n", "repos[0].path: acme/local: expected the path"},
		"same directory twice, one through a symlink": {"repos:\n  - name: acme/a\n    path: " + dir + "\n  - name: acme/b\n    path: " + alias + "\n",
			"repos[1].path: acme/b: "},
	} {
		t.Run(name, func(t *testing.T) {
			o := writeConfig(t, tc.yaml)
			_, err := Load(o)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want it to contain %q", err, tc.want)
			}
			if name == "same directory twice, one through a symlink" && !strings.Contains(err.Error(), "already the checkout of acme/a") {
				t.Fatalf("err = %v; want it to name the other repository", err)
			}
		})
	}
}

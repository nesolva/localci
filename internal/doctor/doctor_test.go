package doctor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeDeps answers commands from a table; a missing entry means "not installed".
func fakeDeps(cmds map[string]string, free uint64, portErr error) Deps {
	return Deps{
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			out, ok := cmds[strings.Join(append([]string{name}, args...), " ")]
			if !ok {
				return "", errors.New("not found")
			}
			if strings.HasPrefix(out, "ERR:") {
				return strings.TrimPrefix(out, "ERR:"), errors.New("exit 1")
			}
			return out, nil
		},
		DiskFree: func(string) (uint64, error) { return free, nil },
		PortFree: func(int) error { return portErr },
	}
}

func byName(rs []Result, name string) Result {
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	return Result{}
}

var healthy = map[string]string{
	"git --version":  "git version 2.50.0",
	"gh --version":   "gh version 2.80.0",
	"gh auth status": "Logged in",
	"docker info --format {{.ServerVersion}}": "29.0.0",
	"mise --version": "2026.1.0",
	"act --version":  "act version 0.2.80",
}

func TestHealthyMachineIsAllOK(t *testing.T) {
	rs := Run(context.Background(), Input{Port: 7357, DataDir: private(t), Repos: 2}, fakeDeps(healthy, 100*gib, nil))
	for _, r := range rs {
		if r.Status != OK || r.Fix != "" {
			t.Errorf("%s: %s %q", r.Name, r.Status, r.Fix)
		}
	}
	if Failed(rs) {
		t.Error("Failed must be false")
	}
}

func TestEachProblemSaysWhatToDo(t *testing.T) {
	cmds := map[string]string{
		"git --version":  "git version 2.50.0",
		"gh --version":   "gh version 2.80.0",
		"gh auth status": "ERR:You are not logged into any GitHub hosts.",
	}
	rs := Run(context.Background(), Input{Port: 7357, DataDir: private(t)}, fakeDeps(cmds, 3*gib, errors.New("in use")))

	want := map[string]Status{
		"repos": Warn, "git": OK,
		"gh auth": Fail, // not signed in
		"docker":  Warn,
		"mise":    Fail, // neither mise nor Docker can provide tool versions
		"disk":    Fail,
		"port":    Warn,
	}
	for name, st := range want {
		r := byName(rs, name)
		if r.Status != st {
			t.Errorf("%s: got %s, want %s (%s)", name, r.Status, st, r.Detail)
		}
		if st != OK && r.Fix == "" && name != "disk" {
			t.Errorf("%s: a %s must say what to do", name, st)
		}
	}
	if !strings.Contains(byName(rs, "gh auth").Fix, "gh auth login") {
		t.Error("gh auth fix must name the command")
	}
	if !Failed(rs) {
		t.Error("Failed must be true")
	}
}

func TestMissingMiseOnlyWarnsWhenDockerWorks(t *testing.T) {
	cmds := map[string]string{}
	for k, v := range healthy {
		cmds[k] = v
	}
	delete(cmds, "mise --version")
	rs := Run(context.Background(), Input{Port: 7357, DataDir: private(t), Repos: 1}, fakeDeps(cmds, 100*gib, nil))
	if r := byName(rs, "mise"); r.Status != Warn {
		t.Errorf("mise: %s", r.Status)
	}
}

func TestMissingActWarnsAndSaysHowToInstall(t *testing.T) {
	cmds := map[string]string{}
	for k, v := range healthy {
		cmds[k] = v
	}
	delete(cmds, "act --version")
	rs := Run(context.Background(), Input{Port: 7357, DataDir: private(t), Repos: 1}, fakeDeps(cmds, 100*gib, nil))
	r := byName(rs, "act")
	if r.Status != Warn || !strings.Contains(r.Fix, "brew install act") {
		t.Errorf("act: %s %q", r.Status, r.Fix)
	}
	if Failed(rs) {
		t.Error("a missing act must only warn")
	}
}

func TestActWithoutDockerWarnsToStartIt(t *testing.T) {
	cmds := map[string]string{}
	for k, v := range healthy {
		cmds[k] = v
	}
	delete(cmds, "docker info --format {{.ServerVersion}}")
	rs := Run(context.Background(), Input{Port: 7357, DataDir: private(t), Repos: 1}, fakeDeps(cmds, 100*gib, nil))
	if r := byName(rs, "act"); r.Status != Warn || !strings.Contains(r.Fix, "start Docker") {
		t.Errorf("act: %s %q", r.Status, r.Fix)
	}
}

func TestLooseDataDirFailsWithTheFix(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := byName(Run(context.Background(), Input{Port: 7357, DataDir: dir, Repos: 1}, fakeDeps(healthy, 100*gib, nil)), "data dir")
	if r.Status != Fail || !strings.Contains(r.Fix, "chmod 700 "+dir) {
		t.Errorf("data dir 0755: %s %q fix %q, want a failure that says how to tighten it", r.Status, r.Detail, r.Fix)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := byName(Run(context.Background(), Input{Port: 7357, DataDir: dir, Repos: 1}, fakeDeps(healthy, 100*gib, nil)), "data dir"); r.Status != OK {
		t.Errorf("data dir 0700: %s %q", r.Status, r.Detail)
	}
}

// private is a data directory the way `serve` leaves it (t.TempDir's own mode depends on the umask).
func private(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestEachRepoPathIsReportedAndMustBeTheTopOfAGitCheckout(t *testing.T) {
	cmds := map[string]string{"git -C /code/ok rev-parse --show-toplevel": "/code/ok", "git -C /code/inner rev-parse --show-toplevel": "/code"}
	for k, v := range healthy {
		cmds[k] = v
	}
	in := Input{Port: 7357, DataDir: private(t), Repos: 3,
		Paths: []RepoPath{{"acme/ok", "/code/ok"}, {"acme/inner", "/code/inner"}, {"acme/plain", "/code/plain"}}}
	rs := Run(context.Background(), in, fakeDeps(cmds, 100*gib, nil))
	if r := byName(rs, "path acme/ok"); r.Status != OK || !strings.Contains(r.Detail, "/code/ok") {
		t.Errorf("a checkout: %+v", r)
	}
	for _, name := range []string{"path acme/inner", "path acme/plain"} {
		if r := byName(rs, name); r.Status != Warn || r.Fix == "" {
			t.Errorf("%s: %+v, want a warning that says what to do", name, r)
		}
	}
}

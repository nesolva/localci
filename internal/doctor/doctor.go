// Package doctor checks that the tools localci relies on are present and
// working, and says what to do about each one that is not.
package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// Status is the outcome of one check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Result is one line of the report.
type Result struct {
	Name   string
	Status Status
	Detail string // what was found
	Fix    string // what to do, empty when ok
}

// Deps is everything doctor touches outside itself, so tests can replace it.
type Deps struct {
	// Run executes a command and returns its combined output.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// DiskFree returns the bytes free on the filesystem holding path.
	DiskFree func(path string) (uint64, error)
	// PortFree reports whether 127.0.0.1:port can be bound.
	PortFree func(port int) error
}

// SystemDeps runs real commands.
func SystemDeps() Deps {
	return Deps{
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		},
		DiskFree: diskFree,
		PortFree: func(port int) error {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return err
			}
			return l.Close()
		},
	}
}

// Input is what doctor needs to know about the configuration.
type Input struct {
	Port    int
	DataDir string
	Repos   int
}

const gib = uint64(1) << 30

// Run performs every check, in a fixed order.
func Run(ctx context.Context, in Input, d Deps) []Result {
	docker := checkDocker(ctx, d)
	return []Result{
		checkRepos(in),
		checkGit(ctx, d),
		checkGH(ctx, d),
		docker,
		checkMise(ctx, d, docker.Status == OK),
		checkAct(ctx, d, docker.Status == OK),
		checkDisk(in.DataDir, d),
		checkPrivate(in.DataDir),
		checkPort(in.Port, d),
	}
}

// Failed reports whether any check failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

func checkRepos(in Input) Result {
	if in.Repos == 0 {
		return Result{"repos", Warn, "no repositories configured",
			"add a `repos:` list to your config file (see `localci config`)"}
	}
	return Result{"repos", OK, fmt.Sprintf("%d configured", in.Repos), ""}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

func checkGit(ctx context.Context, d Deps) Result {
	out, err := d.Run(ctx, "git", "--version")
	if err != nil {
		return Result{"git", Fail, "git not found", "install git (https://git-scm.com/downloads)"}
	}
	return Result{"git", OK, firstLine(out), ""}
}

func checkGH(ctx context.Context, d Deps) Result {
	if _, err := d.Run(ctx, "gh", "--version"); err != nil {
		return Result{"gh", Fail, "GitHub CLI not found", "install it (https://cli.github.com) and run `gh auth login`"}
	}
	if out, err := d.Run(ctx, "gh", "auth", "status"); err != nil {
		return Result{"gh auth", Fail, firstLine(out), "run `gh auth login`"}
	}
	return Result{"gh auth", OK, "signed in", ""}
}

func checkDocker(ctx context.Context, d Deps) Result {
	out, err := d.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return Result{"docker", Warn, "Docker is not running or not installed",
			"start Docker Desktop (or `systemctl start docker`); needed for job containers, services and as the toolchain fallback"}
	}
	return Result{"docker", OK, "server " + firstLine(out), ""}
}

func checkMise(ctx context.Context, d Deps, dockerOK bool) Result {
	out, err := d.Run(ctx, "mise", "--version")
	if err == nil {
		return Result{"mise", OK, firstLine(out), ""}
	}
	if !dockerOK {
		return Result{"mise", Fail, "mise is missing and Docker is not available, so tool versions cannot be provided",
			"install mise (https://mise.jdx.dev) or start Docker"}
	}
	return Result{"mise", Warn, "mise not found: tool versions come from containers",
		"install mise (https://mise.jdx.dev) for faster runs with warm caches"}
}

func checkDisk(dataDir string, d Deps) Result {
	probe := dataDir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	free, err := d.DiskFree(probe)
	if err != nil {
		return Result{"disk", Warn, "cannot read free space: " + err.Error(), ""}
	}
	detail := fmt.Sprintf("%.0f GiB free where logs and workspaces live", float64(free)/float64(gib))
	switch {
	case free < 5*gib:
		return Result{"disk", Fail, detail, "free some space; checks need room for clones and logs"}
	case free < 20*gib:
		return Result{"disk", Warn, detail, "less than 20 GiB free; clean up before it fills"}
	}
	return Result{"disk", OK, detail, ""}
}

// checkPrivate fails when the data directory, or the database and token in it, can be read by other users: the
// token there starts and cancels runs, and logs and checkpoints hold what the tested code printed and stored.
func checkPrivate(dataDir string) Result {
	loose, err := store.LooseModes(dataDir)
	if err != nil {
		return Result{"data dir", Warn, "cannot read its permissions: " + err.Error(), ""}
	}
	if len(loose) == 0 {
		return Result{"data dir", OK, dataDir + " is private to you", ""}
	}
	l := loose[0]
	detail := fmt.Sprintf("%s is mode %04o, others can read it", l.Path, l.Mode)
	if len(loose) > 1 {
		detail += fmt.Sprintf(" (and %d more paths)", len(loose)-1)
	}
	return Result{"data dir", Fail, detail,
		fmt.Sprintf("run `chmod %o %s` (`localci serve` also tightens everything on start)", l.Want, l.Path)}
}

func checkPort(port int, d Deps) Result {
	if err := d.PortFree(port); err != nil {
		return Result{"port", Warn, fmt.Sprintf("127.0.0.1:%d is in use", port),
			"fine if the localci service is already running; otherwise change `server.port`"}
	}
	return Result{"port", OK, fmt.Sprintf("127.0.0.1:%d is free", port), ""}
}

func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

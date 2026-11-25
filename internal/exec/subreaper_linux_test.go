//go:build linux

package exec

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperSubreaper is not a test: the test binary re-executes itself as the two processes of the scenario.
//
//	manager  stands for the service manager of a user service: a subreaper that is not pid 1, and nothing reaps for
//	         it. It runs serve.
//	serve    starts a command whose tool puts a server out of its group and ends.
func TestHelperSubreaper(t *testing.T) {
	switch os.Getenv("LOCALCI_HELPER") {
	case "subreaper-manager":
		syscall.Syscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
		cmd := osexec.Command(os.Args[0], "-test.run=^TestHelperSubreaper$")
		cmd.Env = append(os.Environ(), "LOCALCI_HELPER=subreaper-serve")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Println("serve failed:", err)
		}
		os.Exit(0)
	case "subreaper-serve":
		if os.Getenv("SUBREAPER") == "1" {
			BecomeSubreaper()
		}
		dir := os.Getenv("HELPER_DIR")
		// The server is a child of the command's shell, in a session of its own (so out of the group's signal), and
		// the shell ends while it runs: the server is orphaned.
		p, err := Start(context.Background(), Spec{
			Cmd:         `setsid sleep 300 & echo $! > "` + filepath.Join(dir, "pid") + `"; sleep 0.6`,
			LogPath:     filepath.Join(dir, "run.log"),
			SampleEvery: 30 * time.Millisecond,
			KillGrace:   2 * time.Second,
		})
		if err != nil {
			fmt.Println("start failed:", err)
			os.Exit(0)
		}
		p.Wait()
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		fmt.Printf("RESULT pid=%d alive=%v\n", pid, running(pid))
		os.Exit(0)
	default:
		t.Skip("helper process")
	}
}

// running is true for a process that exists and has not ended (a zombie has).
func running(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}

func runSubreaperScenario(t *testing.T, subreaper string) (pid int, alive bool) {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestHelperSubreaper$")
	cmd.Env = append(os.Environ(), "LOCALCI_HELPER=subreaper-manager", "SUBREAPER="+subreaper, "HELPER_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("scenario: %v\n%s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if _, err := fmt.Sscanf(l, "RESULT pid=%d alive=%t", &pid, &alive); err == nil {
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			return pid, alive
		}
	}
	t.Fatalf("scenario printed no result:\n%s", out)
	return 0, false
}

// Under a Linux user service the process that adopts a run's orphans is the service manager, not pid 1, so a server
// that outlived its tool kept running after the run's cleanup. As a subreaper, serve adopts it and the cleanup ends it.
func TestEscapedGrandchildKilledUnderSubreaper(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	// The premise first, serve not a subreaper: the server survives (this is what the code did before).
	if _, alive := runSubreaperScenario(t, "0"); !alive {
		t.Skip("orphans are adopted by an init that reaps here: the leak does not happen in this environment")
	}
	if pid, alive := runSubreaperScenario(t, "1"); alive {
		t.Fatalf("the server (pid %d) outlived the run's cleanup although serve was a subreaper", pid)
	}
}

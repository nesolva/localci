//go:build unix

package exec

import (
	"context"
	"os"
	osexec "os/exec"
	"sync"
	"syscall"
	"time"
)

func setGroup(cmd *osexec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func groupExists(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

// stopGroup asks every process of the group to end (SIGTERM) and kills what is
// still there after grace (SIGKILL). It returns at once when the group is gone.
func stopGroup(pgid int, grace time.Duration) {
	if !groupExists(pgid) {
		return
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for groupExists(pgid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(-pgid, syscall.SIGKILL)
}

func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// remembered is the processes of a command's tree seen so far, gathered while it runs and when it is stopped. A tool
// may put its server in a group of its own (Playwright does), out of reach of the group's signal; the server is still
// a descendant until its parent dies, so it is noted here and ended with the command.
type remembered struct {
	mu   sync.Mutex
	pids map[int]string // pid -> start time, so a reused pid is not mistaken for the original
}

func (r *remembered) add(rows []psRow, root int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pids == nil {
		r.pids = map[int]string{}
	}
	byPID := map[int]psRow{}
	for _, row := range rows {
		byPID[row.pid] = row
	}
	for _, pid := range treeMembers(rows, root) {
		if pid != os.Getpid() {
			if _, known := r.pids[pid]; !known {
				r.pids[pid] = byPID[pid].start
			}
		}
	}
}

// stopRemembered ends the remembered processes that are still around and still look like the command's own: their
// parent is another remembered process or none (init took them in). SIGTERM first, SIGKILL after grace. A reused
// process id of an unrelated process has neither parent, so it is left alone.
func (r *remembered) stop(grace time.Duration) {
	live := func() []int {
		rows, ok := listProcs(context.Background())
		if !ok {
			return nil
		}
		return r.ours(rows)
	}
	pids := live()
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(grace)
	for len(pids) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		pids = live()
	}
	for _, pid := range pids {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// ours is the pids in rows that are still the remembered processes: same start time as when they were noted, and a
// parent that is another remembered process or none (init took them in).
func (r *remembered) ours(rows []psRow) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int
	for _, row := range rows {
		start, ok := r.pids[row.pid]
		if !ok || start != row.start {
			continue
		}
		if _, parentKnown := r.pids[row.ppid]; row.ppid == 1 || parentKnown {
			out = append(out, row.pid)
		}
	}
	return out
}

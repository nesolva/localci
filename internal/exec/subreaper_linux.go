//go:build linux

package exec

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>.
const prSetChildSubreaper = 36

// BecomeSubreaper makes this process the one that adopts the orphans of its descendants. Under systemd --user (and in
// a container) the process that adopts them is not pid 1, so a run's cleanup, which recognises an escaped process by
// its parent, would never find it; as the subreaper, `localci serve` is that parent. It reports whether it worked.
func BecomeSubreaper() bool {
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return false
	}
	subreaper.Store(true)
	return true
}

// procState is one line of /proc: what reaping needs to know about a process.
type procState struct {
	pid, ppid, pgrp int
	zombie          bool
}

// children lists this process's children by reading /proc, with no process spawned (it runs in short loops).
func children() []procState {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	me := os.Getpid()
	var out []procState
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...: comm may hold spaces and parentheses, so cut after the last ")".
		s := string(b)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 3 {
			continue
		}
		ppid, e1 := strconv.Atoi(f[1])
		pgrp, e2 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || ppid != me {
			continue
		}
		out = append(out, procState{pid: pid, ppid: ppid, pgrp: pgrp, zombie: f[0] == "Z"})
	}
	return out
}

// reapAdopted collects the exit status of the dead processes this subreaper adopted, so they do not stay as zombies
// (init does that for orphans everywhere else). It touches only zombies, and only those of process group pgid or
// among pids: every other child of this process is waited for by os/exec, and taking its status would break that
// Wait. The group's own leader is left out for the same reason. It reports how many were collected.
func reapAdopted(pgid int, pids []int) int {
	if !subreaper.Load() {
		return 0
	}
	n := 0
	for _, c := range children() {
		if !c.zombie || c.pid == pgid || !(c.pgrp == pgid || containsPID(pids, c.pid)) {
			continue
		}
		var ws syscall.WaitStatus
		if got, err := syscall.Wait4(c.pid, &ws, syscall.WNOHANG, nil); err == nil && got == c.pid {
			n++
		}
	}
	return n
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

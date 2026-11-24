package exec

import (
	"bytes"
	"context"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"
)

// Sample is the CPU and memory of a process tree at one moment.
type Sample struct {
	At    time.Time
	CPU   float64 // percent of one core, summed over the tree
	RSSKB int64
	Procs int
}

type psRow struct {
	pid, ppid, pgid int
	cpu             float64
	rssKB           int64
	start           string // process start time (ps lstart): with the pid, it identifies a process
}

// listProcs is every process on the machine, as ps shows it.
func listProcs(ctx context.Context) ([]psRow, bool) {
	cmd := osexec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,pcpu=,rss=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return parsePS(out), true
}

func parsePS(out []byte) []psRow {
	var rows []psRow
	for _, line := range bytes.Split(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 5 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		cpu, e4 := strconv.ParseFloat(f[3], 64)
		rss, e5 := strconv.ParseInt(f[4], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil {
			continue
		}
		rows = append(rows, psRow{pid, ppid, pgid, cpu, rss, strings.Join(f[5:], " ")})
	}
	return rows
}

// treeStats sums root, its descendants and the members of its process group
// (orphans re-parented away from the tree still belong to it).
func treeStats(rows []psRow, root int) Sample {
	byPID := map[int]psRow{}
	for _, r := range rows {
		byPID[r.pid] = r
	}
	var s Sample
	for _, pid := range treeMembers(rows, root) {
		r := byPID[pid]
		s.CPU += r.cpu
		s.RSSKB += r.rssKB
		s.Procs++
	}
	return s
}

// treeMembers lists root, its descendants (whatever group they are in) and the members of its process group.
func treeMembers(rows []psRow, root int) []int {
	children := map[int][]int{}
	byPID := map[int]psRow{}
	for _, r := range rows {
		byPID[r.pid] = r
		children[r.ppid] = append(children[r.ppid], r.pid)
	}
	in := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if in[p] {
			continue
		}
		if _, ok := byPID[p]; !ok {
			continue
		}
		in[p] = true
		queue = append(queue, children[p]...)
	}
	for _, r := range rows {
		if r.pgid == root {
			in[r.pid] = true
		}
	}
	pids := make([]int, 0, len(in))
	for pid := range in {
		pids = append(pids, pid)
	}
	return pids
}

// Package capacity works out what this machine can run: logical CPUs, free
// memory, the Docker VM's memory, and the number of parallel slots that follow.
package capacity

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	gib = uint64(1) << 30

	// memPerCheck is the memory one check is assumed to need until its real cost
	// is measured; dockerMemPerCheck is the same for checks that run containers.
	memPerCheck       = gib * 3 / 2
	dockerMemPerCheck = 2 * gib
	maxSlots          = 32
)

// Machine is a snapshot of what the host offers.
type Machine struct {
	CPUs         int    // logical CPUs
	FreeMemory   uint64 // bytes available without swapping
	Docker       bool   // the Docker daemon answers
	DockerMemory uint64 // bytes the Docker daemon can use (0 when Docker is not running)
	Mise         bool   // mise is on PATH
}

// Detect inspects the host. Missing information degrades to "unknown" (zero)
// rather than failing: a machine we cannot measure still gets a safe minimum.
func Detect(ctx context.Context) Machine {
	m := Machine{CPUs: runtime.NumCPU()}
	m.FreeMemory, _ = freeMemory(ctx)
	m.DockerMemory, m.Docker = dockerMemory(ctx)
	_, err := exec.LookPath("mise")
	m.Mise = err == nil
	return m
}

// Slots returns how many checks and jobs may run at once. A budget of zero
// means "unknown" and does not limit; the result is always at least one.
func Slots(cpus int, memory, dockerMemory uint64) (check, job int) {
	check = maxSlots
	if cpus > 0 {
		check = min(check, cpus)
	}
	if memory > 0 {
		check = min(check, int(memory/memPerCheck))
	}
	if dockerMemory > 0 {
		check = min(check, int(dockerMemory/dockerMemPerCheck))
	}
	check = max(check, 1)
	return check, JobSlots(check)
}

func dockerMemory(ctx context.Context) (uint64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.MemTotal}}").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, true
	}
	return n, true
}

// JobSlots is how many pull request runs may be in progress for a given
// number of check slots: half of them, at least one.
func JobSlots(checkSlots int) int { return max(checkSlots/2, 1) }

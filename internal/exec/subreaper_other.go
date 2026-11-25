//go:build unix && !linux

package exec

// BecomeSubreaper does nothing off Linux: there launchd adopts the orphans of a process, and a run's cleanup already
// finds them by their parent being process 1.
func BecomeSubreaper() bool { return false }

func reapAdopted(pgid int, pids []int) int { return 0 }

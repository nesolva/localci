//go:build unix

package exec

import (
	"os"
	"testing"
)

// A subreaper's cleanup finds the orphans of the command it ran, and only those: another run's command, or one of
// serve's own children, also has serve as its parent but is not in this command's tree.
func TestSubreaperCleanupTakesOnlyWhatTheCommandNoted(t *testing.T) {
	me := os.Getpid()
	var r remembered
	r.add([]psRow{
		{pid: 10, ppid: me, pgid: 10, start: "t0"},
		{pid: 11, ppid: 10, pgid: 11, start: "t1"}, // a server the command's tool put in a group of its own
	}, 10)
	rows := []psRow{
		{pid: 11, ppid: me, pgid: 11, start: "t1"}, // the tool died: the subreaper adopted the server
		{pid: 50, ppid: me, pgid: 50, start: "t2"}, // another run's command: also serve's child
		{pid: 51, ppid: me, pgid: 51, start: "t1"}, // same start time, never noted by this command
	}
	was := subreaper.Swap(false)
	t.Cleanup(func() { subreaper.Store(was) })
	if got := r.ours(rows); len(got) != 0 {
		t.Fatalf("without a subreaper nothing with this process as its parent is ours, got %v", got)
	}
	subreaper.Store(true)
	if got := r.ours(rows); len(got) != 1 || got[0] != 11 {
		t.Fatalf("ours = %v, want only the adopted server (11)", got)
	}
}

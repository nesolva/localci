package suite

import (
	"strings"
	"testing"
)

// A test that fails once and passes when run again does not fail the run, and the run's reason says so: a pass
// that hides a flaky test is worth a line, or nobody ever looks at it.
func TestFlakyTestPassesTheRunAndSaysSo(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"flaky1"}, 4)
	run := g.run(Request{Suite: "e2e", Heads: mainHead()})
	g.mustPass(run)
	if !strings.Contains(run.Reason, "1 flaky") {
		t.Fatalf("reason = %q, want it to say 1 flaky", run.Reason)
	}
	if strings.Contains(run.Reason, MovedNote) {
		t.Fatalf("reason = %q: a test with nothing next to it has no one to conflict with", run.Reason)
	}
}

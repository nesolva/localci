package shard

import (
	"slices"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func at(id TestID, shard int, passed bool, startSec, endSec int) Result {
	return Result{ID: id, Shard: shard, Passed: passed,
		Start: t0.Add(time.Duration(startSec) * time.Second), End: t0.Add(time.Duration(endSec) * time.Second)}
}

func TestDetectConflict(t *testing.T) {
	parallel := []Result{
		at("A", 0, true, 0, 10),
		at("C", 1, true, 12, 20), // overlaps nothing near B
		at("D", 2, true, 8, 11),  // brief overlap with B
		at("B", 1, false, 5, 9),  // fails while A and D run
	}
	alone := []Result{at("B", 0, true, 0, 1), at("B", 0, true, 2, 3)}
	got := Detect(parallel, alone)
	if len(got) != 1 || got[0].ID != "B" {
		t.Fatalf("got %+v, want a conflict on B", got)
	}
	if !slices.Equal(got[0].Evidence, []TestID{"A", "D"}) {
		t.Fatalf("evidence = %v, want [A D] (longest overlap first)", got[0].Evidence)
	}
}

func TestDetectFailingAloneIsNotAConflict(t *testing.T) {
	parallel := []Result{at("A", 0, true, 0, 10), at("B", 1, false, 5, 9)}
	alone := []Result{at("B", 0, false, 0, 1), at("B", 0, true, 2, 3), at("B", 0, true, 4, 5)}
	if got := Detect(parallel, alone); len(got) != 0 {
		t.Fatalf("a test that fails alone is broken, not conflicting: %+v", got)
	}
}

func TestDetectNeedsTwoCleanRunsAndAnOverlap(t *testing.T) {
	overlapping := []Result{at("A", 0, true, 0, 10), at("B", 1, false, 5, 9)}
	if got := Detect(overlapping, []Result{at("B", 0, true, 0, 1)}); len(got) != 0 {
		t.Fatalf("one clean run alone is not proof: %+v", got)
	}
	twice := []Result{at("B", 0, true, 0, 1), at("B", 0, true, 2, 3)}
	if got := Detect([]Result{at("B", 1, false, 5, 9)}, twice); len(got) != 0 {
		t.Fatalf("a failure beside nothing is a flake: %+v", got)
	}
}

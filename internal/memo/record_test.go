package memo

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func flow(r *Recorder, test, step, name string, inputs map[string]any, checkpoint string) (int, string, bool) {
	r.FlowBegin(test, step, name, inputs)
	return r.FlowEnd(test, step, checkpoint)
}

// The prefix is the flows a test calls before its first real step; nested flows are part of their caller.
func TestThePrefixIsTheTopLevelFlowsBeforeTheFirstStep(t *testing.T) {
	r := NewRecorder()
	r.Begin("t")
	r.FlowBegin("t", "t:1", "login", map[string]any{"user": "u"})
	r.FlowBegin("t", "t:2", "create-tenant", nil) // nested: not a node of its own
	r.FlowEnd("t", "t:2", "saved")
	r.FlowEnd("t", "t:1", "saved")
	r.Step("t", "hook") // fixtures set up hooks: not a step of the test
	flow(r, "t", "t:3", "add-product", map[string]any{"sku": "s"}, "restored")
	r.Step("t", "step")
	flow(r, "t", "t:4", "late", nil, "saved") // after the first step: never part of the prefix
	r.End("t", true)
	got := r.Learned()["t"]
	if len(got) != 2 {
		t.Fatalf("prefix = %v, want login then add-product", got)
	}
	// Input order and values decide the signature; the same call signs the same.
	r2 := NewRecorder()
	r2.Begin("u")
	flow(r2, "u", "u:1", "login", map[string]any{"user": "u"}, "saved")
	r2.End("u", true)
	if r2.Learned()["u"][0] != got[0] {
		t.Fatal("the same flow call got two signatures")
	}
	r3 := NewRecorder()
	r3.Begin("v")
	flow(r3, "v", "v:1", "login", map[string]any{"user": "other"}, "saved")
	r3.End("v", true)
	if r3.Learned()["v"][0] == got[0] {
		t.Fatal("different inputs got the same signature")
	}
}

// A flow that keeps no checkpoint cannot be shared, so the prefix stops before it.
func TestThePrefixStopsAtAFlowWithoutACheckpoint(t *testing.T) {
	r := NewRecorder()
	r.Begin("t")
	flow(r, "t", "t:1", "login", nil, "saved")
	flow(r, "t", "t:2", "ephemeral", nil, "none")
	flow(r, "t", "t:3", "after", nil, "saved")
	r.End("t", true)
	if got := r.Learned()["t"]; len(got) != 1 {
		t.Fatalf("prefix = %v, want only login", got)
	}
}

// Only the last attempt of a test that passed teaches anything.
func TestOnlyAPassedTestsLastAttemptIsLearned(t *testing.T) {
	r := NewRecorder()
	r.Begin("flaky")
	flow(r, "flaky", "flaky:1", "wrong", nil, "saved")
	r.End("flaky", false)
	r.Begin("flaky")
	flow(r, "flaky", "flaky:1", "right", nil, "saved")
	r.End("flaky", true)
	r.Begin("bad")
	flow(r, "bad", "bad:1", "login", nil, "saved")
	r.End("bad", false)
	got := r.Learned()
	if _, ok := got["bad"]; ok {
		t.Error("a failed test taught a prefix")
	}
	r2 := NewRecorder()
	r2.Begin("x")
	flow(r2, "x", "x:1", "right", nil, "saved")
	r2.End("x", true)
	if !slices.Equal(got["flaky"], r2.Learned()["x"]) {
		t.Errorf("flaky prefix = %v, want the passing attempt's", got["flaky"])
	}
}

// FlowEnd tells the planner which top-level flow of the test just ended.
func TestFlowEndNamesTheIndexAndSignatureOfTopLevelFlows(t *testing.T) {
	r := NewRecorder()
	r.Begin("t")
	r.FlowBegin("t", "t:1", "a", nil)
	r.FlowBegin("t", "t:2", "inner", nil)
	if _, _, top := r.FlowEnd("t", "t:2", "saved"); top {
		t.Error("a nested flow reported as top level")
	}
	i0, s0, top := r.FlowEnd("t", "t:1", "saved")
	if !top || i0 != 0 {
		t.Fatalf("first top-level flow = (%d, %v)", i0, top)
	}
	r.FlowBegin("t", "t:3", "b", nil)
	i1, s1, _ := r.FlowEnd("t", "t:3", "saved")
	if i1 != 1 || s0 == s1 {
		t.Fatalf("second top-level flow = (%d, %q), first %q", i1, s1, s0)
	}
}

func TestSavedPrefixesSurviveARoundTripAndMerge(t *testing.T) {
	dir := t.TempDir()
	if got, err := Load(dir); err != nil || len(got) != 0 {
		t.Fatalf("empty dir: %v, %v", got, err)
	}
	if err := Save(dir, map[string][]string{"a": {"x"}, "b": {"y"}}); err != nil {
		t.Fatal(err)
	}
	// A later run that ran only "a" and learned a new prefix keeps "b".
	if err := Save(dir, map[string][]string{"a": {"z"}}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil || !slices.Equal(got["a"], []string{"z"}) || !slices.Equal(got["b"], []string{"y"}) {
		t.Fatalf("after merge = %v, %v", got, err)
	}
	// A file localci cannot read plans nothing; it never fails the run.
	os.WriteFile(filepath.Join(dir, "memo-prefixes.json"), []byte("{not json"), 0o600)
	if got, err := Load(dir); err == nil || len(got) != 0 {
		t.Fatalf("corrupt file = %v, %v, want an error and no prefixes", got, err)
	}
}

package flow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

func ev(kind, run, test, step string, data any) store.Event {
	b, _ := json.Marshal(data)
	return store.Event{Kind: kind, Run: run, Test: test, Step: step, Data: b}
}

func TestSummaryFoldsCallsIntoOneLinePerFlowWithRedactedValues(t *testing.T) {
	evs := []store.Event{
		ev("flow.begin", "1", "t1", "s1", map[string]any{"flow": "login", "inputs": map[string]any{"user": "demo", "password": "hunter2"}}),
		ev("flow.end", "1", "t1", "s1", map[string]any{"outputs": map[string]any{"token": "abc", "user": "demo"}, "checkpoint": "saved", "ms": 3000}),
		ev("flow.begin", "1", "t2", "s2", map[string]any{"flow": "login", "inputs": map[string]any{"user": "demo", "password": "hunter2"}}),
		ev("flow.end", "1", "t2", "s2", map[string]any{"outputs": map[string]any{"user": "demo"}, "checkpoint": "restored", "ms": 400}),
		ev("flow.begin", "1", "t2", "s3", map[string]any{"flow": "seed", "inputs": map[string]any{}}),
		ev("flow.end", "1", "t2", "s3", map[string]any{"outputs": map[string]any{}, "checkpoint": "none", "ms": 50}),
	}
	got := Summaries(evs)
	if len(got) != 2 || got[0].Name != "login" || got[1].Name != "seed" {
		t.Fatalf("summaries: %+v", got)
	}
	l := got[0]
	if l.Tests != 2 || l.Calls != 2 || l.RunMS != 3000 || l.RestoreMS != 400 || l.HitRate != 0.5 || l.Checkpointed != 2 {
		t.Errorf("login: %+v", l)
	}
	if got[1].Checkpointed != 0 || got[1].HitRate != 0 {
		t.Errorf("a flow without a checkpoint has no hit rate: %+v", got[1])
	}
	out, _ := json.Marshal(got)
	if strings.Contains(string(out), "hunter2") || strings.Contains(string(out), "abc") {
		t.Errorf("a secret value reached the summary: %s", out)
	}
	if l.Inputs["password"] != "***" {
		t.Errorf("redaction: inputs %v outputs %v", l.Inputs, l.Outputs)
	}
}

// A fork's report is not trusted: its (possibly doctored) flow events must not move the medians a team reads.
func TestForkRunsFlowEventsDoNotReachTheListing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, r := range []store.Run{{ID: "1", Suite: "web", Kind: store.KindSuite, State: store.StatePassed, Created: 10},
		{ID: "2", Suite: "web", Kind: store.KindSuite, State: store.StatePassed, Created: 20, Fork: true}} {
		if err := st.PutRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []store.Event{
		ev("flow.begin", "1", "t1", "s1", map[string]any{"flow": "login", "inputs": map[string]any{}}),
		ev("flow.end", "1", "t1", "s1", map[string]any{"outputs": map[string]any{}, "checkpoint": "saved", "ms": 3000}),
		ev("flow.begin", "2", "t1", "s1", map[string]any{"flow": "login", "inputs": map[string]any{"user": "evil"}}),
		ev("flow.end", "2", "t1", "s1", map[string]any{"outputs": map[string]any{}, "checkpoint": "saved", "ms": 1}),
	} {
		e.TS = 1
		if _, err := st.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.KindEvents(ctx, 100, Kinds...)
	if err != nil {
		t.Fatal(err)
	}
	got := Summaries(evs)
	if len(got) != 1 || got[0].Calls != 1 || got[0].RunMS != 3000 || got[0].Inputs["user"] != nil {
		t.Errorf("the fork run's events reached the summary: %+v", got)
	}
}

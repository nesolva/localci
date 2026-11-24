package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"testing"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/store"
)

func TestRestartCancelsLeftoversWithAnEventAndAFinalStatus(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	heads, _ := json.Marshal([]forge.Head{{Repo: "acme/web", Branch: "main", SHA: "abc123", PR: 7}})
	for _, r := range []store.Run{
		{ID: "1", Suite: "e2e", Kind: store.KindSuite, Heads: heads, State: store.StateRunning},
		{ID: "2", Suite: "e2e", Kind: store.KindSuite, State: store.StateQueued}, // a manual run: no pull request
		{ID: "3", Suite: "e2e", Kind: store.KindSuite, Heads: heads, State: store.StatePassed},
	} {
		if err := st.PutRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	fg := &recordForge{}
	elog := event.NewLog(st, event.NewBus())
	if err := cancelLeftovers(ctx, st, elog, fg, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}

	got := fg.statuses()
	if len(got) != 1 || got[0].sha != "abc123" || got[0].st.State != forge.StateError || got[0].st.Description != restartReason {
		t.Fatalf("statuses = %+v; want one error on the PR head of run 1", got)
	}
	evs, _ := st.EventsAfter(ctx, 0, 100)
	cancelled := map[string]bool{}
	for _, e := range evs {
		var d struct{ State string }
		if e.Kind == "run.state" && json.Unmarshal(e.Data, &d) == nil && d.State == store.StateCancelled {
			cancelled[e.Run] = true
		}
	}
	if !cancelled["1"] || !cancelled["2"] || cancelled["3"] {
		t.Fatalf("run.state cancelled events for %v; want runs 1 and 2 only", cancelled)
	}
	if r, _ := st.Run(ctx, "3"); r.State != store.StatePassed {
		t.Fatalf("a finished run changed to %s", r.State)
	}
}

// An affected run leaves its own pending status, localci/<suite> (affected); clearing the suite's would leave the
// affected one pending for ever and wrongly fail the full suite's.
func TestRestartClearsTheAffectedStatusOfAnAffectedRun(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	heads, _ := json.Marshal([]forge.Head{{Repo: "acme/web", Branch: "feat", SHA: "abc123", PR: 7}})
	if err := st.PutRun(ctx, store.Run{ID: "1", Suite: "e2e", Kind: store.KindSuite, Heads: heads, State: store.StateRunning, Affected: true}); err != nil {
		t.Fatal(err)
	}
	fg := &recordForge{}
	if err := cancelLeftovers(ctx, st, event.NewLog(st, event.NewBus()), fg, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	got := fg.statuses()
	if len(got) != 1 || forge.Context(got[0].st.Suite) != "localci/e2e (affected)" {
		t.Fatalf("statuses = %+v; want one on localci/e2e (affected)", got)
	}
}

// A check whose final state reached the event log (and GitHub) but not the run
// row must not be reported as "localci restarted" over its real result.
func TestRestartKeepsAResultTheEventLogAlreadyHas(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	heads, _ := json.Marshal([]forge.Head{{Repo: "acme/api", SHA: "abc123", PR: 7}})
	if err := st.PutRun(ctx, store.Run{ID: "1", Suite: "CI/build", Kind: store.KindCheck, Heads: heads, State: store.StateRunning}); err != nil {
		t.Fatal(err)
	}
	elog := event.NewLog(st, event.NewBus())
	data, _ := json.Marshal(map[string]string{"state": store.StateFailed, "reason": "1 step failed"})
	if _, err := elog.Append(ctx, store.Event{Run: "1", Kind: "run.state", Data: data}); err != nil {
		t.Fatal(err)
	}
	fg := &recordForge{}
	if err := cancelLeftovers(ctx, st, elog, fg, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	if got := fg.statuses(); len(got) != 0 {
		t.Fatalf("statuses = %+v; the real result is already on the pull request", got)
	}
	if r, _ := st.Run(ctx, "1"); r.State != store.StateFailed {
		t.Fatalf("run state = %s, want the logged %s", r.State, store.StateFailed)
	}
}

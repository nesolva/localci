package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
)

func TestTriageEndpointListsClassifiedTestsOfAPassedRunToo(t *testing.T) {
	srv, st := newServer(t)
	ctx := t.Context()
	heads := json.RawMessage(`[{"repo":"acme/shop","sha":"abc","fork":true}]`)
	if err := st.PutRun(ctx, store.Run{ID: "7", Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed, Heads: heads}); err != nil {
		t.Fatal(err)
	}
	put := func(id, status, class, ev string) {
		tc := store.Test{Run: "7", Shard: 1, ID: id, File: "a.spec.ts", Title: id, Status: status, Class: class}
		if ev != "" {
			tc.Evidence = json.RawMessage(ev)
		}
		if err := st.PutTest(ctx, tc); err != nil {
			t.Fatal(err)
		}
	}
	put("plain", "passed", "", "")
	put("wobbly", "passed", "flaky", `{"lines":["failed 1, passed 1"],"note":"failed on the way"}`)
	put("fixme", "failed", "test-bug", `{"lines":["a","b"],"patch":{"path":"/x.patch","file":"a.spec.ts","shas":["abc"],"from":"Pay","to":"Place order"}}`)

	var rep TriageReport
	getJSON(t, srv.URL+"/api/runs/7/triage", &rep)
	if !rep.Fork || rep.Propose != "patch" || len(rep.Tests) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	by := map[string]TriageTest{}
	for _, tc := range rep.Tests {
		by[tc.Test] = tc
	}
	if w := by["wobbly"]; w.Test != "wobbly" || w.Class != "flaky" || w.Note == "" {
		t.Fatalf("flaky test of a passed run = %+v", w)
	}
	if f := by["fixme"]; f.Patch == nil || f.Patch.To != "Place order" || len(f.Lines) != 2 {
		t.Fatalf("test bug = %+v", f)
	}
}

func TestFlakyEndpointCountsRunsInTheWindow(t *testing.T) {
	srv, st := newServer(t)
	ctx := t.Context()
	now := time.Now().UnixMilli()
	for i, created := range []int64{now - 1000, now - 2000, now - 60*24*3600*1000} {
		id := string(rune('a' + i))
		st.PutRun(ctx, store.Run{ID: id, Suite: "e2e", Kind: store.KindSuite, State: store.StatePassed, Created: created})
		st.PutTest(ctx, store.Test{Run: id, Shard: 1, ID: "t1", File: "a.spec.ts", Title: "wobbly", Status: "passed", Class: "flaky"})
	}
	var l FlakyList
	getJSON(t, srv.URL+"/api/flaky?days=30", &l)
	if len(l.Tests) != 1 || l.Tests[0].Runs != 2 {
		t.Fatalf("list = %+v: the run from 60 days ago is out of the window", l)
	}
	if resp := do(t, "GET", srv.URL+"/api/flaky?days=x", "", ""); resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

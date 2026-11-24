package server

import (
	"testing"

	"github.com/nesolva/localci/internal/store"
)

// Shard 0 is the serial group and the reruns: an agent reading a run or a failure must be told so, not left to
// guess what shard 0 is.
func TestShardZeroIsLabelledSerial(t *testing.T) {
	srv, st := newServer(t)
	ctx := t.Context()
	if err := st.PutRun(ctx, store.Run{ID: "7", Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1} {
		if err := st.PutShard(ctx, store.Shard{Run: "7", N: n, State: "failed", LogPath: "/logs/s.log"}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutTest(ctx, store.Test{Run: "7", Shard: n, ID: "t" + itoa(int64(n)), File: "a.spec.ts", Title: "t" + itoa(int64(n)),
			Status: "failed"}); err != nil {
			t.Fatal(err)
		}
	}

	var d RunDetail
	getJSON(t, srv.URL+"/api/runs/7", &d)
	got := map[int]string{}
	for _, sh := range d.ShardList {
		got[sh.N] = sh.Label
	}
	if len(got) != 2 || got[0] != "serial" || got[1] != "" {
		t.Errorf("shard labels = %v, want 0:serial and 1:empty", got)
	}

	var fl FailureList
	getJSON(t, srv.URL+"/api/runs/7/failures", &fl)
	byShard := map[int]string{}
	for _, f := range fl.Failures {
		byShard[f.Shard] = f.Label
	}
	if len(byShard) != 2 || byShard[0] != "serial" || byShard[1] != "" {
		t.Errorf("failure labels = %v, want 0:serial and 1:empty", byShard)
	}
}

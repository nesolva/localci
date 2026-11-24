package main

import (
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/server"
)

// show tells an agent which shards the run had and calls shard 0 what it is, so a failure on "serial" can be
// found in the list and its log opened.
func TestShowListsTheShardsAndCallsShardZeroSerial(t *testing.T) {
	run := failedRun()
	serial := checkoutFailure()
	serial.Shard, serial.Label = 0, "serial"
	routes := map[string]any{
		"GET /api/runs/42": server.RunDetail{Run: run, ShardList: []server.ShardInfo{
			{N: 0, State: "failed", Label: "serial", LogPath: "/data/logs/42/shard-0.log"},
			{N: 1, State: "passed", LogPath: "/data/logs/42/shard-1.log"},
		}},
		"GET /api/runs/42/failures": server.FailureList{Run: run, Total: 1, Failures: []server.Failure{serial}},
	}
	out, errOut, code := cli(t, fake(t, routes), "show", "42")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"· serial ·",
		"serial · failed · /data/logs/42/shard-0.log",
		"shard 1 · passed · /data/logs/42/shard-1.log",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output lacks %q:\n%s", want, out)
		}
	}

	out, _, _ = cli(t, fake(t, routes), "show", "42", "--json")
	if !strings.Contains(out, `{"type":"shard","n":0,"state":"failed","label":"serial","log_path":"/data/logs/42/shard-0.log"}`) {
		t.Errorf("show --json lacks the labelled shard:\n%s", out)
	}
}

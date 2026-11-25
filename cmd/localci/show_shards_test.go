package main

import (
	"strings"
	"testing"
	"unicode"

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

// Everything a failure says comes from a report the code under test wrote (a fork's pull request included): an escape
// sequence in it must reach the agent's terminal as harmless text, never clear the screen or plant a hidden link.
func TestShowOutputCarriesNoTerminalEscapes(t *testing.T) {
	const esc = "\x1b[2J\x1b]8;;https://evil.test\x07"
	run := failedRun()
	a := checkoutFailure()
	a.Title = "pays " + esc + "here\x1b]8;;\x07"
	a.Location = "tests/\x1b[2Jcart.spec.ts:41"
	a.Class = "product-bug" + esc
	a.Step = "click " + esc + "Pay"
	a.Expected, a.Actual = "enabled\x1b[31m", "disabled \u202egnirts\u202c"
	a.Aria = "- button \"Pay\" " + esc + "\n- link \"Back\" \u009b2J"
	a.Console = []string{"TypeError " + esc + "card"}
	a.Network = []server.NetworkError{{Method: "POST" + esc, URL: "/api/payments" + esc, Status: 500}}
	a.Screenshot, a.Trace = "/data/s.png"+esc, "/data/trace.zip"+esc
	b := secondFailure()
	b.File, b.Message = "tests/\x1b[2Jcart.spec.ts", "locator \x1b[31mresolved\x1b[0m to 0 elements"
	routes := map[string]any{
		"GET /api/runs/42":          server.RunDetail{Run: run},
		"GET /api/runs/42/failures": server.FailureList{Run: run, Total: 2, Failures: []server.Failure{a, b}},
	}
	for _, args := range [][]string{{"show", "42", "--all"}, {"show", "42", "--all", "--full"}} {
		out, errOut, code := cli(t, fake(t, routes), args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errOut)
		}
		if i := strings.IndexFunc(out, func(r rune) bool {
			return r != '\n' && (unicode.IsControl(r) || r == '\u202e' || r == '\u202c')
		}); i >= 0 {
			t.Fatalf("%v printed a control character %q:\n%q", args, []rune(out[i:])[0], out)
		}
		for _, want := range []string{"pays", "click", "enabled", "disabled", "resolved", "TypeError", "/api/payments", "trace.zip",
			"  aria: - button", "\n        - link \"Back\""} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: the visible text %q is gone:\n%s", args, want, out)
			}
		}
	}
}

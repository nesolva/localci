package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/store"
)

func putFlowCall(t *testing.T, st *store.Store, test, step, flow string, inputs, outputs map[string]any, checkpoint string, ms int) {
	t.Helper()
	ctx := context.Background()
	b, _ := json.Marshal(map[string]any{"flow": flow, "inputs": inputs})
	e, _ := json.Marshal(map[string]any{"outputs": outputs, "checkpoint": checkpoint, "ms": ms})
	if _, err := st.AppendEvent(ctx, store.Event{Run: "7", Kind: "flow.begin", Test: test, Step: step, Data: b}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendEvent(ctx, store.Event{Run: "7", Kind: "flow.end", Test: test, Step: step, Data: e}); err != nil {
		t.Fatal(err)
	}
}

func flowsServer(t *testing.T) string {
	url, st := realServer(t, nil)
	putFlowCall(t, st, "t1", "t1:f1", "login", map[string]any{"user": "demo", "password": "hunter2"}, map[string]any{"user": "demo"}, "saved", 3200)
	putFlowCall(t, st, "t2", "t2:f1", "login", map[string]any{"user": "demo", "password": "hunter2"}, map[string]any{"user": "demo"}, "restored", 400)
	putFlowCall(t, st, "t3", "t3:f1", "login", map[string]any{"user": "demo", "password": "hunter2"}, map[string]any{"user": "demo"}, "restored", 600)
	putFlowCall(t, st, "t1", "t1:f2", "create-tenant", map[string]any{"name": "Acme"}, map[string]any{"slug": "acme"}, "none", 900)
	return url
}

func TestFlowsListsEveryFlowWithItsUseAndCheckpointHitRate(t *testing.T) {
	out, code := realCLI(t, flowsServer(t), "flows")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	golden(t, "flows.txt", out)
}

func TestFlowsJSONRedactsSecretInputs(t *testing.T) {
	out, code := realCLI(t, flowsServer(t), "flows", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(out, "hunter2") || !strings.Contains(out, `"password":"***"`) {
		t.Errorf("the password input must show as ***: %s", out)
	}
	golden(t, "flows.json", out)
}

func TestFlowsWithoutRunsSaysHowToListDeclaredFlows(t *testing.T) {
	url, _ := realServer(t, nil)
	out, code := realCLI(t, url, "flows")
	if code != 0 || !strings.Contains(out, "no flow has run yet") {
		t.Errorf("exit %d: %q", code, out)
	}
}

package suite

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// An exploratory run gets what a test shard gets (the workspace of the default branch, a database of its own, a
// running stack), runs the agent instead of tests, and ends passed whatever the agent found. It teaches nothing.
func TestExploreRunGetsAnIsolatedShardAndIsNotAPullRequestRun(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a", "b"}, 4)
	var seen ExploreEnv
	var stackUp, dbThere bool
	run := g.run(Request{Suite: "e2e", Trigger: ExploreTrigger, RequestedBy: "test", Explore: &ExploreRequest{
		BaseURL: `http://127.0.0.1:{{port "app"}}`,
		Run: func(ctx context.Context, env ExploreEnv) (string, error) {
			seen = env
			_, err := os.Stat(env.Dir + "/tests.txt")
			stackUp = err == nil && len(pidsIn(g.shared+"/stack.pids")) == 1
			dbThere = len(g.dbFiles()) == 1
			return "2 findings", nil
		}}})
	if run.State != "passed" || run.Reason != "2 findings" {
		t.Fatalf("run = %s %q", run.State, run.Reason)
	}
	if !strings.HasPrefix(seen.BaseURL, "http://127.0.0.1:") || strings.Contains(seen.BaseURL, "{{") {
		t.Errorf("base url = %q", seen.BaseURL)
	}
	if !stackUp || !dbThere {
		t.Errorf("while the agent ran: workspace and one stack %v, one shard database %v", stackUp, dbThere)
	}
	if !strings.Contains(strings.Join(seen.Env, "\n"), "LOCALCI_DB_URL=") {
		t.Errorf("the agent's environment has no database: %v", seen.Env)
	}
	waitFor(t, "stack and database torn down", func() bool {
		return len(g.dbFiles()) == 0 && !alive(pidsIn(g.shared + "/stack.pids")[0])
	})
	if tests, _ := g.st.RunTests(context.Background(), run.ID); len(tests) != 0 {
		t.Errorf("an exploratory run records no tests: %d", len(tests))
	}
	if sh, _ := g.st.Shards(context.Background(), run.ID); len(sh) != 1 || sh[0].N != 1 {
		t.Errorf("shards = %+v", sh)
	}
}

func TestExploreNeverTakesAPullRequestOrAFilter(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 4)
	ex := &ExploreRequest{BaseURL: "http://x", Run: func(context.Context, ExploreEnv) (string, error) { return "", nil }}
	for name, req := range map[string]Request{
		"a pull request head": {Suite: "e2e", Heads: prHead(5), Explore: ex},
		"a fork":              {Suite: "e2e", Fork: true, Explore: ex},
		"a test filter":       {Suite: "e2e", Tests: Filter{Grep: "a"}, Explore: ex},
	} {
		if _, err := g.r.Submit(req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// What stops the exploration from happening is a failed run; what the agent says is not.
func TestExploreRunFailsOnlyWhenTheAgentCouldNotRun(t *testing.T) {
	g := newRig(t, fakeSuite(""), []string{"a"}, 4)
	run := g.run(Request{Suite: "e2e", Trigger: ExploreTrigger, Explore: &ExploreRequest{BaseURL: "http://x",
		Run: func(context.Context, ExploreEnv) (string, error) { return "", fmt.Errorf("agent command: not found") }}})
	if run.State != "failed" || !strings.Contains(run.Reason, "not found") {
		t.Errorf("run = %s %q", run.State, run.Reason)
	}
}

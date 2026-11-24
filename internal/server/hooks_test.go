package server

import (
	"net/http"
	"testing"
)

// Test packages call the hooks with their run's hook token, never the API token, so every hook area gets through
// the guard with that token, and nothing else does: without it a hook is refused, and the run's token opens no
// other write.
func TestEveryHookAreaPassesWithItsRunsTokenAndNothingElseDoes(t *testing.T) {
	r := newRigWith(t, func(d *Deps) { d.Hooks = oneRun{"7", "run-seven"} })
	for _, path := range []string{"/runs/7/hooks/test/begin", "/runs/7/hooks/flow/lookup", "/runs/7/hooks/flow/save", "/runs/7/hooks/flow/restore", "/runs/7/hooks/memo/next"} {
		if resp := do(t, http.MethodPost, r.tcp.URL+path+"?test=t", "run-seven", ""); resp.StatusCode == http.StatusForbidden {
			t.Errorf("%s with its run's token answered 403", path)
		}
		if resp := do(t, http.MethodPost, r.tcp.URL+path+"?test=t", "", ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s without a token answered %d, want 403", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/api/runs", "/runs/7/hooks", "/runs/7/hooks/flow", "/runs/7/hooks/flow/a/b", "/api/runs/7/hooks/flow/lookup"} {
		if resp := do(t, http.MethodPost, r.tcp.URL+path, "run-seven", ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s with a run's hook token answered %d, want 403", path, resp.StatusCode)
		}
	}
}

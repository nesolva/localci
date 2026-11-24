package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nesolva/localci/internal/suite"
)

// workerRig serves the worker handler over TLS, as the real listener does.
func workerRig(t *testing.T) (url string, srv *Server, r *rig) {
	t.Helper()
	r = newRig(t)
	srv = New(Deps{Store: r.st, DataDir: t.TempDir(), Token: testToken})
	ts := httptest.NewTLSServer(srv.WorkerHandler())
	t.Cleanup(ts.Close)
	return ts.URL, srv, r
}

func post(t *testing.T, url, cred string, body any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// The join token buys one credential; a wrong or used token is refused; a revoked credential stops working at its
// next call; the local API token is worth nothing on the worker listener.
func TestWorkerListenerRefusals(t *testing.T) {
	url, srv, _ := workerRig(t)
	tok, err := srv.d.Store.NewJoinToken(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := post(t, url+"/worker/v1/join", "", JoinRequest{Token: "wrong", Name: "sim-1", Capacity: 2}); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	code, body := post(t, url+"/worker/v1/join", "", JoinRequest{Token: tok, Name: "sim-1", Capacity: 2})
	if code != 200 {
		t.Fatalf("join: %d %s", code, body)
	}
	var jr JoinResponse
	json.Unmarshal(body, &jr)
	if code, _ := post(t, url+"/worker/v1/join", "", JoinRequest{Token: tok, Name: "sim-2", Capacity: 2}); code != 401 {
		t.Fatalf("used token: %d", code)
	}
	if code, _ := post(t, url+"/worker/v1/heartbeat", jr.Credential, Heartbeat{Capacity: 2}); code != 200 {
		t.Fatalf("heartbeat with its credential: %d", code)
	}
	for _, c := range []struct{ name, cred, path string }{
		{"no credential", "", "/worker/v1/heartbeat"},
		{"the local API token", testToken, "/worker/v1/heartbeat"},
	} {
		if code, _ := post(t, url+c.path, c.cred, Heartbeat{}); code != 401 {
			t.Errorf("%s: %d, want 401", c.name, code)
		}
	}
	// Nothing but the worker routes and run hooks is mounted: not the API, not the dashboard, and a hook call
	// carrying the API token (not the run's hook token) is refused.
	for _, p := range []string{"/api/runs", "/", "/api/workers/token"} {
		if code, _ := post(t, url+p, testToken, nil); code != 404 {
			t.Errorf("POST %s: %d, want 404", p, code)
		}
	}
	if code, _ := post(t, url+"/runs/r1/hooks/test/begin", testToken, nil); code != 403 {
		t.Errorf("hook with the API token: %d, want 403", code)
	}
	if err := srv.d.Store.RevokeWorker(t.Context(), "sim-1"); err != nil {
		t.Fatal(err)
	}
	if code, _ := post(t, url+"/worker/v1/heartbeat", jr.Credential, Heartbeat{}); code != 401 {
		t.Fatalf("revoked credential: %d, want 401", code)
	}
}

// The large limit belongs to the artifacts route of a shard, not to any path that happens to end in /artifacts.
func TestBodyLimitIsPerRouteNotPerSuffix(t *testing.T) {
	for _, p := range []string{
		"/runs/r1/hooks/artifacts",
		"/worker/v1/shards/r/1/artifacts",
		"/worker/v1/shards/r/1/1/x/artifacts",
		"/worker/v1/join/artifacts",
		"/worker/v1/git/o/artifacts",
	} {
		if got := bodyLimit(p); got >= suite.MaxArtifactBytes {
			t.Errorf("%s: limit %d is the artifacts limit", p, got)
		}
	}
}

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

const testToken = "s3cret"

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	r := newRig(t)
	return r.tcp, r.st
}

// rig is one server answering on both listeners: TCP (httptest) and a Unix socket.
type rig struct {
	tcp  *httptest.Server
	sock *http.Client // dials the socket whatever the URL says
	st   *store.Store
}

const sockURL = "http://localci"

func newRig(t *testing.T) *rig { return newRigWith(t, nil) }

func newRigWith(t *testing.T, mutate func(*Deps)) *rig {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// A short path: t.TempDir() can exceed the socket path limit.
	dir, err := os.MkdirTemp("", "lc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := Deps{Store: st, Bus: event.NewBus(), DataDir: dir, Token: testToken}
	if mutate != nil {
		mutate(&d)
	}
	srv := New(d)
	tcp := httptest.NewServer(srv.Handler())
	t.Cleanup(tcp.Close)
	l, path, err := ListenSocket(dir)
	if err != nil {
		t.Fatal(err)
	}
	ss := httptest.NewUnstartedServer(srv.SocketHandler())
	ss.Listener.Close()
	ss.Listener = l
	ss.Start()
	t.Cleanup(ss.Close)
	return &rig{tcp: tcp, sock: socketClient(path), st: st}
}

// socketClient dials the socket whatever the URL says.
func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
}

func do(t *testing.T, method, url, token, host string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestWritesNeedTheTokenAndReadsDoNot(t *testing.T) {
	r := newRig(t)
	srv := r.tcp
	for _, c := range []struct {
		name, method, path, token string
		want                      int
	}{
		{"read without token", "GET", "/api/runs", "", 200},
		{"write without token", "POST", "/api/runs/1/cancel", "", 403},
		{"write with a wrong token", "POST", "/api/runs/1/cancel", "nope", 403},
		{"write with the token reaches the handler", "POST", "/api/runs/1/cancel", testToken, 404},
		{"submit without token", "POST", "/api/runs", "", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			// reads on TCP, writes on the socket
			var got int
			if c.method == "GET" {
				got = do(t, c.method, srv.URL+c.path, c.token, "").StatusCode
			} else {
				got = doVia(t, r.sock, c.method, sockURL+c.path, c.token).StatusCode
			}
			if got != c.want {
				t.Errorf("%s %s: status %d, want %d", c.method, c.path, got, c.want)
			}
		})
	}
}

func doVia(t *testing.T, hc *http.Client, method, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// The token never has a use over TCP: a write there is refused whatever it carries and the answer says where to
// go; the same call over the socket goes through.
func TestWritesOverTCPAreRefusedEvenWithTheToken(t *testing.T) {
	Handle("POST /api/test-extension", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(func() { extraMu.Lock(); extra = nil; extraMu.Unlock() })
	const secret = "/home/someone/private/data/api.sock"
	r := newRigWith(t, func(d *Deps) { d.Socket = secret })

	resp := do(t, "POST", r.tcp.URL+"/api/test-extension", testToken, "")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "writes go through the local socket: use the localci CLI") {
		t.Errorf("TCP write: %d %s, want 403 pointing at the CLI", resp.StatusCode, body)
	}
	if strings.Contains(string(body), secret) || strings.Contains(string(body), "/") {
		t.Errorf("the TCP refusal leaks a path: %s", body)
	}
	if got := doVia(t, r.sock, "POST", sockURL+"/api/test-extension", testToken).StatusCode; got != 200 {
		t.Errorf("socket write: %d, want 200", got)
	}
}

// A page on another origin can make the browser call 127.0.0.1 under its own host name; the server refuses it.
func TestRequestsForAnotherHostAreRefused(t *testing.T) {
	srv, _ := newServer(t)
	if got := do(t, "GET", srv.URL+"/api/runs", "", "evil.example:7357").StatusCode; got != 403 {
		t.Errorf("status %d, want 403", got)
	}
	if got := do(t, "GET", srv.URL+"/api/runs", "", "localhost:7357").StatusCode; got != 200 {
		t.Errorf("localhost: status %d, want 200", got)
	}
}

func TestServerListensOnLoopbackOnly(t *testing.T) {
	l, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	host, _, _ := net.SplitHostPort(l.Addr().String())
	if host != "127.0.0.1" {
		t.Errorf("listening on %s, want 127.0.0.1", host)
	}
}

func TestExtraRoutesMountBehindTheSameGuards(t *testing.T) {
	Handle("POST /api/test-extension", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	t.Cleanup(func() { extraMu.Lock(); extra = nil; extraMu.Unlock() })
	r := newRig(t)
	if got := doVia(t, r.sock, "POST", sockURL+"/api/test-extension", "").StatusCode; got != 403 {
		t.Errorf("without token: %d, want 403", got)
	}
	if got := doVia(t, r.sock, "POST", sockURL+"/api/test-extension", testToken).StatusCode; got != 200 {
		t.Errorf("with token: %d, want 200", got)
	}
}

// Only a hook call (POST /runs/<run>/hooks/<area>/<action>, any area) is recognised as one; its token is then
// checked against the run's by the suite runner.
func TestHookPathFamily(t *testing.T) {
	for path, want := range map[string]string{
		"/runs/42/hooks/test/begin":       "42",
		"/runs/42/hooks/test/end":         "42",
		"/runs/42/hooks/flow/save":        "42",
		"/runs/42/hooks/flow/lookup":      "42",
		"/runs/42/hooks/test/begin/more":  "",
		"/api/runs/42/hooks/test/begin":   "",
		"/runs/42/cancel":                 "",
		"/api/runs/42/cancel":             "",
		"/runs/42/hooks/test":             "",
		"/runs/42/hooks/flow":             "",
		"/runs//hooks/test/begin":         "",
		"/runs/../api/runs/hooks/test/x/": "",
	} {
		if got, _ := hookRun(path); got != want {
			t.Errorf("hookRun(%q) = %q, want %q", path, got, want)
		}
	}
}

// A hook call without a token, or with one that is not its run's, never reaches the runner; there is no run on
// the server here, so even the right shape of call is refused.
func TestHookCallsAreRefusedWithoutTheirRunsToken(t *testing.T) {
	r := newRig(t)
	for name, token := range map[string]string{"no token": "", "the API token": testToken, "a made-up token": "abc"} {
		for _, viaSocket := range []bool{false, true} {
			var got int
			if viaSocket {
				got = doVia(t, r.sock, "POST", sockURL+"/runs/1/hooks/test/begin?test=x", token).StatusCode
			} else {
				got = do(t, "POST", r.tcp.URL+"/runs/1/hooks/test/begin?test=x", token, "").StatusCode
			}
			if got != 403 {
				t.Errorf("%s (socket=%v): %d, want 403", name, viaSocket, got)
			}
		}
	}
}

type oneRun struct{ run, token string }

func (o oneRun) HookTokenValid(run, token string) bool { return run == o.run && token == o.token }

// The right token of the right run gets a hook call through the guard, on either listener; another run's token
// does not.
func TestHookCallWithItsRunsTokenPassesTheGuard(t *testing.T) {
	Handle("POST /runs/{run}/hooks/{area}/{action}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(func() { extraMu.Lock(); extra = nil; extraMu.Unlock() })
	r := newRigWith(t, func(d *Deps) { d.Hooks = oneRun{"7", "run-seven"} })
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/runs/7/hooks/test/begin?test=x", "run-seven", 204},
		{"/runs/7/hooks/flow/save", "run-seven", 204},
		{"/runs/8/hooks/test/begin?test=x", "run-seven", 403}, // its token, another run
		{"/runs/7/hooks/test/begin?test=x", "run-eight", 403},
	} {
		if got := do(t, "POST", r.tcp.URL+c.path, c.token, "").StatusCode; got != c.want {
			t.Errorf("TCP %s with %s: %d, want %d", c.path, c.token, got, c.want)
		}
	}
}

func seed(t *testing.T, st *store.Store, id string, titles ...string) {
	t.Helper()
	ctx := t.Context()
	if err := st.PutRun(ctx, store.Run{ID: id, Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed}); err != nil {
		t.Fatal(err)
	}
	for i, title := range titles {
		fail := json.RawMessage(`{"step":"click","message":"boom","expected":"a","actual":"b"}`)
		tid := id + "-" + title
		if err := st.PutTest(ctx, store.Test{Run: id, Shard: 1, ID: tid, File: "a.spec.ts", Title: title, Line: i + 1,
			Status: "failed", Failure: fail}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendEvent(ctx, store.Event{Run: id, Kind: "test.end", Test: tid}); err != nil {
			t.Fatal(err)
		}
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp := do(t, "GET", url, "", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// The cursor a call prints is the event number to pass back: only what happened after it comes back.
func TestSinceReturnsOnlyWhatIsNewerThanTheCursor(t *testing.T) {
	srv, st := newServer(t)
	seed(t, st, "1", "first")
	var first RunList
	getJSON(t, srv.URL+"/api/runs?since=0", &first)
	if len(first.Runs) != 1 || first.Cursor == 0 {
		t.Fatalf("first call: %+v", first)
	}

	seed(t, st, "2", "second")
	var next RunList
	getJSON(t, srv.URL+"/api/runs?since="+itoa(first.Cursor), &next)
	if len(next.Runs) != 1 || next.Runs[0].ID != "2" {
		t.Errorf("runs since the cursor: %+v, want only run 2", next.Runs)
	}

	// failures: a second failing test of run 1 appears after the cursor
	var fl FailureList
	getJSON(t, srv.URL+"/api/runs/1/failures", &fl)
	cursor := fl.Cursor
	seed(t, st, "1", "late")
	getJSON(t, srv.URL+"/api/runs/1/failures?since="+itoa(cursor), &fl)
	if len(fl.Failures) != 1 || fl.Failures[0].Title != "late" || fl.Total != 2 {
		t.Errorf("failures since the cursor: %+v", fl)
	}
	if fl.Failures[0].Expected != "a" || fl.Failures[0].Step != "click" {
		t.Errorf("failure report not flattened: %+v", fl.Failures[0])
	}
}

func TestBadSinceIsAUsageError(t *testing.T) {
	srv, _ := newServer(t)
	resp := do(t, "GET", srv.URL+"/api/runs?since=yesterday", "", "")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "30m") {
		t.Errorf("status %d body %s, want 400 that explains the format", resp.StatusCode, b)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A log line longer than a line reader's buffer (a minified bundle, a JSON dump) must not hide the end of the log:
// the tail is the last lines of the file, and a client reading them line by line, as `localci logs` does, gets
// through to the last one.
func TestLogTailReachesTheEndPastAVeryLongLine(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	path := t.TempDir() + "/shard-1.log"
	body := "first\n" + strings.Repeat("x", 2<<20) + "\nafter 1\nafter 2\nError: the end\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.st.PutRun(ctx, store.Run{ID: "7", Suite: "e2e", Kind: store.KindSuite, State: store.StateFailed}); err != nil {
		t.Fatal(err)
	}
	if err := r.st.PutShard(ctx, store.Shard{Run: "7", N: 1, State: store.StateFailed, LogPath: path}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tail string
		want []string
	}{
		{"3", []string{"after 1", "after 2", "Error: the end"}},
		{"10", []string{"first", "x", "after 1", "after 2", "Error: the end"}},
	} {
		resp := do(t, "GET", r.tcp.URL+"/api/runs/7/log?tail="+tc.tail, "", "")
		var got []string
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20) // what `localci logs` reads with
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "xxx") {
				l = "x" // the long line, whole or cut
			}
			got = append(got, l)
		}
		if err := sc.Err(); err != nil {
			t.Errorf("tail=%s: a line reader stops at %v after %q", tc.tail, err, got)
			continue
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("tail=%s: got %q, want %q", tc.tail, got, tc.want)
		}
	}
}

// Package server is localci's local HTTP surface: the JSON API the agent CLI speaks, the dashboard, the event
// stream and the hooks the test package calls. The API, writes included, is served on a Unix socket in the private
// data directory and needs the token kept there; 127.0.0.1 serves only the dashboard, read-only calls and the
// hooks, which carry a per-run token.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nesolva/localci/internal/capacity"
	"github.com/nesolva/localci/internal/checks"
	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/explore"
	"github.com/nesolva/localci/internal/flow"
	"github.com/nesolva/localci/internal/forge"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
	"github.com/nesolva/localci/internal/web"
)

// Deps is what the server reads and drives. Suites and Checks may be nil: their calls then answer that nothing is
// configured.
type Deps struct {
	Store  *store.Store
	Bus    *event.Bus
	Suites *suite.Runner
	Checks *checks.Runner
	// Explore starts exploratory runs; nil when the config has no `explore` section. ExploreCtx bounds the runs it follows.
	Explore    *explore.Service
	ExploreCtx context.Context
	Sched      *sched.Scheduler
	Config     func() (*config.Effective, error)
	Machine    capacity.Machine
	DataDir    string
	Token      string
	// Hooks says whether a token is a run's hook token; nil means the suite runner's.
	Hooks interface{ HookTokenValid(run, token string) bool }
	// Report follows a suite run submitted outside the dispatcher (a retry) and posts its status on the pull
	// requests of heads until it ends. Nil when nothing posts statuses.
	Report func(run, name string, heads []forge.Head, since int64)
	// Socket is where the API socket listens.
	Socket string
	// WorkerCert, WorkerFingerprint, WorkerPort and WorkerListen describe the worker TLS listener (nil cert: none).
	// LostAfter is how long a silent worker keeps its shards; zero means DefaultLostAfter.
	WorkerCert        *tls.Certificate
	WorkerFingerprint string
	WorkerPort        int
	WorkerListen      string
	LostAfter         time.Duration
	// Workers tracks connected workers and hands them shards; it is also the suite runner's Remote. Nil: New makes one.
	Workers *WorkerHub
	// GitRepos are the repositories whose mirrors workers may fetch from (those of the configured suites).
	GitRepos []string
}

var (
	extraMu sync.Mutex
	extra   []route
)

type route struct {
	pattern string
	h       http.Handler
}

// Handle registers an extra route, in the form http.ServeMux takes ("GET /api/flows"). Packages call it from their
// own files in init(); every server made afterwards mounts it behind the same host and token checks.
func Handle(pattern string, h http.Handler) {
	extraMu.Lock()
	defer extraMu.Unlock()
	extra = append(extra, route{pattern, h})
}

// Server is the HTTP side of a running localci.
type Server struct {
	d         Deps
	handler   http.Handler // 127.0.0.1: dashboard, GET routes, hooks
	sock      http.Handler // the Unix socket: everything, writes with the token
	workerSrv http.Handler // the worker TLS listener: worker credentials and run hooks
	hub       *WorkerHub
}

// New builds the server. Nothing is listening yet: see Listen.
func New(d Deps) *Server {
	if d.Hooks == nil && d.Suites != nil {
		d.Hooks = d.Suites
	}
	if d.Workers == nil {
		d.Workers = NewWorkerHub(d.LostAfter)
	}
	s := &Server{d: d, hub: d.Workers}
	s.workerSrv = s.buildWorkerServer()
	mux := http.NewServeMux()
	s.routes(mux)
	extraMu.Lock()
	for _, r := range extra {
		mux.Handle(r.pattern, r.h)
	}
	extraMu.Unlock()

	var opts []web.Option
	if d.Config != nil {
		opts = append(opts, web.WithConfig(d.Config))
	}
	opts = append(opts, web.WithMachine(d.Machine))
	mux.Handle("/", web.Handler(d.Store, d.Bus, opts...))
	if d.Suites != nil {
		mux.Handle("POST /runs/{run}/hooks/", d.Suites.Handler())
	}
	mux.Handle("GET /api/flows", flow.APIHandler(d.Store))
	s.handler = s.guard(mux, true)
	s.sock = s.guard(mux, false)
	return s
}

// Handler is what the TCP listener serves: the dashboard, GET routes and the hooks. Writes are answered 403.
func (s *Server) Handler() http.Handler { return s.handler }

// SocketHandler is what the Unix socket serves: the whole API, writes with the token.
func (s *Server) SocketHandler() http.Handler { return s.sock }

// Listen binds 127.0.0.1:port (port 0 picks a free one).
func Listen(port int) (net.Listener, error) {
	return net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}

// Serve answers on the TCP listener l until ctx is done, then stops accepting and drains briefly.
func (s *Server) Serve(ctx context.Context, l net.Listener) error { return serve(ctx, l, s.handler) }

// ServeSocket answers on the Unix socket listener l, in the same way.
func (s *Server) ServeSocket(ctx context.Context, l net.Listener) error { return serve(ctx, l, s.sock) }

func serve(ctx context.Context, l net.Listener, h http.Handler) error {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	done := make(chan error, 1)
	go func() { done <- hs.Serve(l) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
		hs.Close()
		<-done
		return nil
	}
}

// guard keeps out requests meant for another host name (a web page resolving its own name to 127.0.0.1) and
// writes that are not allowed. A hook call needs the token of the run it is for. Any other write needs the API
// token, and only over the socket: the TCP listener answers it 403 whatever token it carries, so the token never
// has a reason to travel there.
func (s *Server) guard(next http.Handler, tcp bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tcp && !localHost(r.Host) {
			fail(w, http.StatusForbidden, "this server answers only to 127.0.0.1 and localhost")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if run, ok := hookRun(r.URL.Path); ok && r.Method == http.MethodPost {
				if s.d.Hooks == nil || !s.d.Hooks.HookTokenValid(run, bearer(r)) {
					fail(w, http.StatusForbidden, "hook calls need the token of their run (Authorization: Bearer $LOCALCI_HOOK_TOKEN)")
					return
				}
			} else if tcp {
				fail(w, http.StatusForbidden, "writes go through the local socket: use the localci CLI")
				return
			} else if !tokenMatches(s.d.Token, bearer(r)) {
				fail(w, http.StatusForbidden, "writes need the token in %s (Authorization: Bearer <token>)", TokenFile(s.d.DataDir))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hookRun returns the run of a hook call, /runs/<run>/hooks/<area>/<action>. The whole family of an area is hooks.
func hookRun(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/runs/")
	if !ok {
		return "", false
	}
	// Exactly /runs/<run>/hooks/<area>/<action>: every hook area (test, flow, …) is called by the test package.
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[0] == "" || parts[1] != "hooks" || parts[2] == "" || parts[3] == "" {
		return "", false
	}
	return parts[0], true
}

func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
}

func localHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, Error{Error: fmt.Sprintf(format, args...)})
}

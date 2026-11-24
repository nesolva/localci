package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// A worker is another machine that runs shards for this one. It talks to its own TLS listener, never to the
// dashboard port or the socket: the listener serves /worker/v1/ (worker credentials) and the run hooks (the run's hook
// token), and nothing else. The local API token is not accepted there.

// Worker protocol timing: a worker silent for LostAfter loses its shards.
const (
	DefaultLostAfter  = 60 * time.Second
	maxWorkerBodySize = 64 << 20
)

type workerCtxKey struct{}

// workerName returns the authenticated worker of a request served by the worker listener.
func workerName(r *http.Request) string {
	n, _ := r.Context().Value(workerCtxKey{}).(string)
	return n
}

// WorkerHandler is what the worker TLS listener serves.
func (s *Server) WorkerHandler() http.Handler { return s.workerSrv }

// ListenWorker binds the worker listener: 127.0.0.1 unless listen is "lan", then every interface.
func ListenWorker(listen string, port int) (net.Listener, error) {
	host := "127.0.0.1"
	if listen == "lan" {
		host = "0.0.0.0"
	}
	return net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
}

// ServeWorkers answers on the worker listener l over TLS with the certificate in Deps.WorkerCert.
func (s *Server) ServeWorkers(ctx context.Context, l net.Listener) error {
	cfg := &tls.Config{Certificates: []tls.Certificate{*s.d.WorkerCert}, MinVersion: tls.VersionTLS13}
	return serve(ctx, tls.NewListener(l, cfg), s.workerSrv)
}

func (s *Server) buildWorkerServer() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /worker/v1/join", s.workerJoin)
	mux.HandleFunc("POST /worker/v1/heartbeat", s.workerBeat)
	mux.HandleFunc("POST /worker/v1/claim", s.workerClaim)
	mux.HandleFunc("POST /worker/v1/shards/{run}/{shard}/{gen}/{what}", s.workerShardOut)
	mux.HandleFunc("GET /worker/v1/git/{owner}/{repo}/info/refs", s.workerGit)
	mux.HandleFunc("POST /worker/v1/git/{owner}/{repo}/git-upload-pack", s.workerGit)
	if s.d.Suites != nil {
		mux.Handle("POST /runs/{run}/hooks/", s.d.Suites.Handler())
	}
	return s.workerGuard(mux)
}

// bodyLimit is how much one worker call may send. The artifacts of a shard travel as one tar, so that route allows
// what the sink accepts (plus the tar headers); every other call stays small. The route is matched whole
// (/worker/v1/shards/{run}/{shard}/{gen}/artifacts), not by how a path ends.
func bodyLimit(path string) int64 {
	if p := strings.Split(path, "/"); len(p) == 8 && p[0] == "" && p[1] == "worker" && p[2] == "v1" && p[3] == "shards" &&
		p[4] != "" && p[5] != "" && p[6] != "" && p[7] == "artifacts" {
		return suite.MaxArtifactBytes + 8<<20
	}
	return maxWorkerBodySize
}

// workerGuard is the third guard mode. /worker/v1/join is open to anyone holding a join token (the token is the
// credential, checked in the handler); the run hooks need their run's hook token, exactly as on the local port;
// every other /worker/v1/ call, reads included, needs a live worker credential. Everything else is a 404: the
// dashboard and the API are not on this listener.
func (s *Server) workerGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/worker/v1/join":
		case strings.HasPrefix(p, "/worker/v1/"):
			w2, err := s.d.Store.WorkerByCredential(r.Context(), bearer(r))
			if err != nil {
				fail(w, http.StatusUnauthorized, "worker credential refused (revoked or unknown): join again with a new token")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), workerCtxKey{}, w2.Name))
		default:
			if run, ok := hookRun(p); ok && r.Method == http.MethodPost {
				if s.d.Hooks == nil || !s.d.Hooks.HookTokenValid(run, bearer(r)) {
					fail(w, http.StatusForbidden, "hook calls need the token of their run")
					return
				}
			} else {
				fail(w, http.StatusNotFound, "not found")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(p))
		next.ServeHTTP(w, r)
	})
}

// JoinRequest is what a worker sends to join. The token is the single-use secret printed by `localci workers token`.
type JoinRequest struct {
	Token    string `json:"token"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

// JoinResponse carries the worker's long-lived credential, shown once.
type JoinResponse struct {
	Credential string `json:"credential"`
}

func (s *Server) workerJoin(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "bad join request")
		return
	}
	cred, err := s.d.Store.Join(r.Context(), req.Token, req.Name, req.Capacity)
	switch {
	case errors.Is(err, store.ErrBadToken):
		fail(w, http.StatusUnauthorized, "%v", err)
	case errors.Is(err, store.ErrNameTaken):
		fail(w, http.StatusConflict, "%v", err)
	case errors.Is(err, store.ErrBadName):
		fail(w, http.StatusBadRequest, "%v", err)
	case err != nil:
		fail(w, http.StatusInternalServerError, "join: %v", err)
	default:
		s.hub.beat(req.Name, req.Capacity)
		writeJSON(w, http.StatusOK, JoinResponse{Credential: cred})
	}
}

// Heartbeat is what a worker sends every few seconds: its capacity and the shard keys it is running.
type Heartbeat struct {
	Capacity int      `json:"capacity"`
	Running  []string `json:"running"`
}

// HeartbeatResponse lists the shard keys the worker must stop (cancelled, or already requeued).
type HeartbeatResponse struct {
	Drop []string `json:"drop"`
}

func (s *Server) workerBeat(w http.ResponseWriter, r *http.Request) {
	var hb Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		fail(w, http.StatusBadRequest, "bad heartbeat")
		return
	}
	name := workerName(r)
	if err := s.d.Store.TouchWorker(r.Context(), name, hb.Capacity); errors.Is(err, store.ErrBadCredential) {
		fail(w, http.StatusUnauthorized, "worker credential refused (revoked or unknown): join again with a new token")
		return
	}
	drop, ok := s.hub.heartbeat(name, hb.Capacity, hb.Running)
	if !ok {
		fail(w, http.StatusUnauthorized, "worker credential refused (revoked or unknown): join again with a new token")
		return
	}
	writeJSON(w, http.StatusOK, HeartbeatResponse{Drop: drop})
}

// TokenResponse is what POST /api/workers/token answers.
type TokenResponse struct {
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	Listen      string `json:"listen"`
}

func (s *Server) workerToken(w http.ResponseWriter, r *http.Request) {
	if s.d.WorkerCert == nil || s.d.WorkerPort == 0 {
		fail(w, http.StatusServiceUnavailable, "the worker listener is not running (see the serve log)")
		return
	}
	tok, err := s.d.Store.NewJoinToken(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, TokenResponse{Token: tok, Fingerprint: s.d.WorkerFingerprint, Port: s.d.WorkerPort, Listen: s.d.WorkerListen})
}

func (s *Server) workerRevoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.d.Store.RevokeWorker(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNoWorker) {
			fail(w, http.StatusNotFound, "%v", err)
		} else {
			fail(w, http.StatusInternalServerError, "%v", err)
		}
		return
	}
	s.hub.dropWorker(name)
	writeJSON(w, http.StatusOK, map[string]string{"revoked": name})
}

// remoteWorkers lists registered workers for GET /api/workers.
func (s *Server) remoteWorkers(ctx context.Context) []Worker {
	rows, err := s.d.Store.Workers(ctx)
	if err != nil {
		return nil
	}
	var out []Worker
	for _, row := range rows {
		x := Worker{Name: row.Name, Remote: true, Capacity: row.Capacity, LastSeen: row.LastSeen, Status: "offline"}
		if row.Revoked {
			x.Status = "revoked"
		} else if lw, ok := s.hub.snapshot(row.Name); ok {
			x.Status, x.Capacity, x.Running = "online", lw.capacity, len(lw.shards)
			x.Shards = lw.shards
		}
		out = append(out, x)
	}
	return out
}

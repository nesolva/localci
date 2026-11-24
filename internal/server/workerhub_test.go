package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

func claimOne(t *testing.T, h *WorkerHub, worker string) <-chan error {
	t.Helper()
	res := make(chan error, 1)
	go func() {
		_, err := h.RunShard(context.Background(), suite.RemoteJob{Run: "1", Shard: 1, Gen: 1}, nil)
		res <- err
	}()
	if h.claim(context.Background(), worker, 2*time.Second) == nil {
		t.Fatal("the worker was given no shard")
	}
	return res
}

func TestAShardHeldByAWorkerThatHeartbeatsWithoutRunningItIsRequeued(t *testing.T) {
	now := time.Unix(1000, 0)
	h := NewWorkerHub(time.Minute)
	h.now = func() time.Time { return now }
	h.beat("w1", 1)
	res := claimOne(t, h, "w1")

	// a heartbeat right after the claim lists nothing yet: the shard keeps its grace
	h.heartbeat("w1", 1, nil)
	select {
	case err := <-res:
		t.Fatalf("a fresh claim was requeued: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// later the worker still lists nothing (restarted, lost the claim answer): the shard goes back
	now = now.Add(45 * time.Second)
	h.heartbeat("w1", 1, nil)
	select {
	case err := <-res:
		if !errors.Is(err, suite.ErrWorkerLost) {
			t.Fatalf("err = %v, want ErrWorkerLost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a shard held by a heartbeating worker that does not run it was never requeued")
	}
}

func TestAShardTheWorkerListsAsRunningStaysAssigned(t *testing.T) {
	now := time.Unix(1000, 0)
	h := NewWorkerHub(time.Minute)
	h.now = func() time.Time { return now }
	h.beat("w1", 1)
	res := claimOne(t, h, "w1")
	now = now.Add(45 * time.Second)
	if drop, _ := h.heartbeat("w1", 1, []string{keyOf("1", 1, 1)}); len(drop) != 0 {
		t.Fatalf("drop = %v", drop)
	}
	select {
	case err := <-res:
		t.Fatalf("a shard the worker runs was requeued: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAdvertisedWorkerCapacityHasAnUpperBound(t *testing.T) {
	h := NewWorkerHub(time.Minute)
	h.beat("w1", 1<<30)
	if lw, _ := h.snapshot("w1"); lw.capacity != store.MaxWorkerCapacity {
		t.Fatalf("capacity = %d", lw.capacity)
	}
}

func TestWorkerCallsAllowAnArtifactTarAsBigAsTheSinkAcceptsAndNothingElseAsBig(t *testing.T) {
	if got := bodyLimit("/worker/v1/shards/r/1/1/artifacts"); got < suite.MaxArtifactBytes {
		t.Fatalf("artifacts limit %d is below what the sink accepts (%d)", got, suite.MaxArtifactBytes)
	}
	if got := bodyLimit("/worker/v1/shards/r/1/1/report"); got >= suite.MaxArtifactBytes {
		t.Fatalf("report limit %d is not small", got)
	}
}

// A heartbeat that was already past the credential check when the worker was revoked must not bring it back: the
// primary refuses it and keeps no live entry for the name.
func TestRevokedWorkerHeartbeatCreatesNothing(t *testing.T) {
	_, srv, _ := workerRig(t)
	tok, _ := srv.d.Store.NewJoinToken(t.Context())
	if _, err := srv.d.Store.Join(t.Context(), tok, "w1", 2); err != nil {
		t.Fatal(err)
	}
	srv.hub.beat("w1", 2)
	if err := srv.d.Store.RevokeWorker(t.Context(), "w1"); err != nil {
		t.Fatal(err)
	}
	srv.hub.dropWorker("w1")

	req := httptest.NewRequest("POST", "/worker/v1/heartbeat", strings.NewReader(`{"capacity":2}`))
	req = req.WithContext(context.WithValue(req.Context(), workerCtxKey{}, "w1"))
	rec := httptest.NewRecorder()
	srv.workerBeat(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat of a revoked worker: %d, want 401", rec.Code)
	}
	if _, ok := srv.hub.snapshot("w1"); ok {
		t.Fatal("a heartbeat re-created the revoked worker's live entry")
	}
	srv.hub.mu.Lock()
	_, present := srv.hub.live["w1"]
	srv.hub.mu.Unlock()
	if present {
		t.Fatal("live entry exists for a revoked worker")
	}
}

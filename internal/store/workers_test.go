package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// A join token buys exactly one worker even when two machines race with it, and a join that is refused for its name
// does not burn the token.
func TestJoinTokenIsSingleUseAndRefusedJoinKeepsIt(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "db"))
	tok, err := s.NewJoinToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Join(ctx, tok, "bad name!", 2); !errors.Is(err, ErrBadName) {
		t.Fatalf("bad name: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, n := range []string{"w-a", "w-b", "w-c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Join(ctx, tok, n, 2); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrBadToken) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d joins won with one token", ok)
	}
	tok2, _ := s.NewJoinToken(ctx)
	ws, _ := s.Workers(ctx)
	if _, err := s.Join(ctx, tok2, ws[0].Name, 1); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := s.Join(ctx, tok2, "w-z", 1); err != nil {
		t.Fatalf("token burned by a refused join: %v", err)
	}
}

// A revoked credential stops working and its name stays taken.
func TestRevokedWorkerCredentialStopsWorking(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "db"))
	tok, _ := s.NewJoinToken(ctx)
	cred, err := s.Join(ctx, tok, "sim-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := s.WorkerByCredential(ctx, cred); err != nil || w.Name != "sim-1" || w.Capacity != 3 {
		t.Fatalf("%+v %v", w, err)
	}
	if err := s.RevokeWorker(ctx, "sim-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkerByCredential(ctx, cred); !errors.Is(err, ErrBadCredential) {
		t.Fatalf("revoked credential accepted: %v", err)
	}
	tok2, _ := s.NewJoinToken(ctx)
	if _, err := s.Join(ctx, tok2, "sim-1", 1); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("revoked name reused: %v", err)
	}
}

func TestJoinAndTouchClampTheAdvertisedCapacity(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "db"))
	ctx := context.Background()
	tok, _ := st.NewJoinToken(ctx)
	cred, err := st.Join(ctx, tok, "w-big", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := st.WorkerByCredential(ctx, cred)
	if w.Capacity != MaxWorkerCapacity {
		t.Fatalf("join capacity = %d", w.Capacity)
	}
	st.TouchWorker(ctx, "w-big", 1<<30)
	w, _ = st.WorkerByCredential(ctx, cred)
	if w.Capacity != MaxWorkerCapacity {
		t.Fatalf("touched capacity = %d", w.Capacity)
	}
}

// A bad token gets the same answer whatever the name, so the join call cannot be used to list worker names.
func TestJoinChecksTheTokenBeforeTheName(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "db"))
	tok, _ := s.NewJoinToken(ctx)
	if _, err := s.Join(ctx, tok, "taken", 1); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"taken", "free", "bad name!"} {
		if _, err := s.Join(ctx, "not-a-token", name, 1); !errors.Is(err, ErrBadToken) {
			t.Errorf("bad token with name %q: %v, want ErrBadToken", name, err)
		}
	}
	// A spent token is a bad token too.
	if _, err := s.Join(ctx, tok, "taken", 1); !errors.Is(err, ErrBadToken) {
		t.Errorf("spent token with a taken name: %v, want ErrBadToken", err)
	}
}

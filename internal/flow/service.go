package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// Service answers the flow hooks and keeps the checkpoints.
type Service struct {
	Store   *store.Store
	DataDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per suite and key: one save, restore or eviction of a checkpoint at a time
}

// Attach registers the flow hooks with the suite runner.
func Attach(r *suite.Runner, s *Service) { r.HandleHook("flow", s.Hook) }

func (s *Service) mutex(suiteName, key string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	m := s.locks[suiteName+"/"+key]
	if m == nil {
		m = new(sync.Mutex)
		s.locks[suiteName+"/"+key] = m
	}
	return m
}

func (s *Service) lock(suiteName, key string) func() {
	m := s.mutex(suiteName, key)
	m.Lock()
	return m.Unlock
}

// tryLock is lock that gives up when the key is in use (a save or a restore holds it): ok says it was taken.
func (s *Service) tryLock(suiteName, key string) (unlock func(), ok bool) {
	m := s.mutex(suiteName, key)
	if !m.TryLock() {
		return nil, false
	}
	return m.Unlock, true
}

// maxBody bounds a save: a storage state and outputs are small, a megabyte of cookies already is a bug.
const maxBody = 16 << 20

// Hook is the handler of the flow area: lookup, save and restore.
func (s *Service) Hook(w http.ResponseWriter, req *http.Request, hr suite.HookRun, action string) {
	if hr.Test.Test == "" {
		fail(w, http.StatusBadRequest, "use /hooks/flow/%s?test=<id>", action)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxBody)
	switch action {
	case "lookup":
		var in lookupReq
		if !decode(w, req, &in) {
			return
		}
		out, err := s.lookup(req.Context(), hr, in)
		reply(w, out, err)
	case "save":
		var in saveReq
		if !decode(w, req, &in) {
			return
		}
		out, err := s.save(req.Context(), hr, in)
		reply(w, out, err)
	case "restore":
		var in restoreReq
		if !decode(w, req, &in) {
			return
		}
		out, err := s.restore(req.Context(), hr, in)
		reply(w, out, err)
	default:
		fail(w, http.StatusNotFound, "no such flow hook %q (lookup, save, restore)", action)
	}
}

type lookupReq struct {
	Flow    string `json:"flow"`
	Source  string `json:"source"`  // hash of the flow's source and declaration
	Inputs  string `json:"inputs"`  // hash of the inputs
	Parent  string `json:"parent"`  // key of what ran before in this test, "" for the first flow
	Browser bool   `json:"browser"` // the flow keeps browser storage state
	DB      bool   `json:"db"`      // and a database snapshot
}

type lookupResp struct {
	Key string `json:"key"`
	// Hit: a finished checkpoint exists and restoring it is allowed.
	Hit bool `json:"hit"`
	// Save: after running the flow the package should send it to save.
	Save   bool   `json:"save"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) lookup(ctx context.Context, hr suite.HookRun, in lookupReq) (lookupResp, error) {
	if in.Flow == "" || in.Source == "" {
		return lookupResp{}, badRequest("lookup needs flow and source")
	}
	out := lookupResp{Key: Key(hr.Suite, in.Flow, in.Source, in.Inputs, hr.Build, hr.Template, in.Parent)}
	switch {
	case !in.Browser && !in.DB:
		out.Reason = "the flow declares no checkpoint"
		return out, nil
	case hr.Fork:
		out.Reason = "runs of a fork save no checkpoints"
		return out, nil
	case in.DB && hr.Remote:
		out.Reason = "this shard runs on another machine"
		return out, nil
	case in.DB && hr.DB == nil:
		out.Reason = "the suite has no database to checkpoint"
		return out, nil
	case in.DB && hr.Isolation != "isolated":
		// A shared database holds other shards' data: restoring one shard's snapshot would erase it.
		out.Reason = fmt.Sprintf("database checkpoints are restored only with isolated shards (this suite uses %s)", hr.Isolation)
		return out, nil
	}
	c, err := s.Store.Checkpoint(ctx, hr.Suite, out.Key)
	switch {
	case err == nil:
		if why, definite := s.unusable(ctx, hr, c); why != "" {
			// Forgotten only for a definite cause, and only when nothing holds the key: a save or a restore of it may
			// be cloning the snapshot this very moment.
			if unlock, ok := s.tryLock(hr.Suite, c.Key); ok {
				if definite {
					s.forget(ctx, hr, c)
				}
				unlock()
			}
			out.Reason = why
		} else {
			out.Hit, out.Save = true, !hr.NoSave
			return out, nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return out, err
	}
	// A miss: the flow runs and is saved. A database flow first gets the shard's database put back to where it
	// starts, so its snapshot holds only what it wrote, not what earlier tests on this shard left.
	if in.DB {
		ok, err := s.start(ctx, hr, in.Parent)
		if err != nil {
			return out, err
		}
		if !ok {
			out.Reason = "the flow before it in this test has no database checkpoint, so this one runs without saving"
			return out, nil
		}
	}
	out.Save = !hr.NoSave
	return out, nil
}

// unusable says why a recorded checkpoint cannot be restored, "" when it can. A checkpoint whose files or database
// vanished (cleaned by hand, a database server that was recreated) is forgotten, never half restored: that is a
// definite cause. A check that could not be made (the database server did not answer) is not, and forgets nothing.
func (s *Service) unusable(ctx context.Context, hr suite.HookRun, c store.Checkpoint) (why string, definite bool) {
	path, err := s.file(hr.Suite, c.Key)
	if err != nil {
		return err.Error(), false
	}
	if _, err := readFile(path); err != nil {
		return "the checkpoint's files are gone, running the flow again", true
	}
	if c.Database != "" {
		if hr.DB == nil {
			return "the suite has no database to restore", false
		}
		complete, known, err := shard.IsComplete(ctx, hr.DB, c.Database)
		if err != nil {
			return "cannot check the checkpoint's database: " + err.Error(), false
		}
		if known && !complete {
			return "the checkpoint's database is missing or was never finished, running the flow again", true
		}
	}
	return "", false
}

// forget drops a checkpoint that cannot be used.
func (s *Service) forget(ctx context.Context, hr suite.HookRun, c store.Checkpoint) {
	if c.Database != "" && hr.DB != nil {
		_ = hr.DB.Drop(context.WithoutCancel(ctx), c.Database)
	}
	if path, err := s.file(hr.Suite, c.Key); err == nil {
		removeFile(path)
	}
	_ = s.Store.DeleteCheckpoint(context.WithoutCancel(ctx), hr.Suite, c.Key)
}

type saveReq struct {
	Key  string `json:"key"`
	Flow string `json:"flow"`
	// Source, Inputs and Parent are what lookup was given: save derives the key from them and this run's commits
	// and template, so a run only ever writes checkpoints for its own commits.
	Source       string          `json:"source"`
	Inputs       string          `json:"inputs"`
	Parent       string          `json:"parent"`
	Browser      bool            `json:"browser"`
	DB           bool            `json:"db"`
	StorageState json.RawMessage `json:"storageState"`
	Outputs      json.RawMessage `json:"outputs"`
	// Replace overwrites a checkpoint that exists: the package sets it after a restored checkpoint failed verify.
	Replace bool `json:"replace"`
}

type saveResp struct {
	Saved  bool   `json:"saved"`
	Reason string `json:"reason,omitempty"`
}

func (s *Service) save(ctx context.Context, hr suite.HookRun, in saveReq) (saveResp, error) {
	if !ValidKey(in.Key) || in.Flow == "" {
		return saveResp{}, badRequest("save needs the key lookup returned and the flow's name")
	}
	if in.Key != Key(hr.Suite, in.Flow, in.Source, in.Inputs, hr.Build, hr.Template, in.Parent) {
		return saveResp{}, badRequest("the key is not this run's key for the flow, its source, inputs and parent")
	}
	if hr.Fork {
		return saveResp{Reason: "runs of a fork save no checkpoints"}, nil
	}
	if hr.NoSave {
		return saveResp{Reason: "the run is an infrastructure failure, so it saves no checkpoints"}, nil
	}
	if in.DB && hr.Remote {
		return saveResp{Reason: "this shard runs on another machine"}, nil
	}
	if in.DB && (hr.DB == nil || hr.Isolation != "isolated") {
		return saveResp{Reason: "database checkpoints are saved only with isolated shards"}, nil
	}
	defer s.lock(hr.Suite, in.Key)()
	if c, err := s.Store.Checkpoint(ctx, hr.Suite, in.Key); err == nil {
		if why, _ := s.unusable(ctx, hr, c); !in.Replace && why == "" {
			return saveResp{Saved: true}, nil // another shard saved it first
		}
		s.forget(ctx, hr, c)
	}
	path, err := s.file(hr.Suite, in.Key)
	if err != nil {
		return saveResp{}, err
	}
	c := store.Checkpoint{Suite: hr.Suite, Key: in.Key, Flow: in.Flow}
	undo := func() {
		removeFile(path)
		if c.Database != "" {
			_ = hr.DB.Drop(context.WithoutCancel(ctx), c.Database)
		}
	}
	if c.Bytes, err = writeFile(path, payload{StorageState: in.StorageState, Outputs: in.Outputs}); err != nil {
		return saveResp{}, err
	}
	if in.DB {
		from := hr.Test.DB
		if from == "" {
			undo()
			return saveResp{Reason: "this test has no shard database to snapshot"}, nil
		}
		c.Database = CheckpointDB(in.Key)
		if err := hr.DB.Snapshot(ctx, from, c.Database); err != nil {
			undo()
			return saveResp{}, fmt.Errorf("database snapshot: %w", err)
		}
		c.Bytes += shard.SizeOf(ctx, hr.DB, c.Database)
	}
	limit := s.maxSize(hr)
	if c.Bytes > limit {
		undo()
		return saveResp{Reason: fmt.Sprintf("the checkpoint (%s) is larger than checkpoints.max_size (%s)", size(c.Bytes), size(limit))}, nil
	}
	// The row is the completion mark: until it exists nothing finds the checkpoint.
	if err := s.Store.PutCheckpoint(ctx, c); err != nil {
		undo()
		return saveResp{}, err
	}
	s.evict(ctx, hr, limit, in.Key)
	return saveResp{Saved: true}, nil
}

type restoreReq struct {
	Key string `json:"key"`
	// Reset puts an isolated shard's database back to what the flow starts from instead of restoring the
	// checkpoint: the checkpoint of Parent, or the suite's template when there is no parent. The package asks for it
	// when a restored flow's verify failed and it is going to run the flow again.
	Reset  bool   `json:"reset"`
	Parent string `json:"parent"`
}

type restoreResp struct {
	// Reset: the shard's database is back where the flow starts, so the flow may run again and be saved.
	Reset        bool            `json:"reset,omitempty"`
	Outputs      json.RawMessage `json:"outputs,omitempty"`
	StorageState json.RawMessage `json:"storageState,omitempty"`
}

func (s *Service) restore(ctx context.Context, hr suite.HookRun, in restoreReq) (restoreResp, error) {
	if !ValidKey(in.Key) {
		return restoreResp{}, badRequest("restore needs the key lookup returned")
	}
	if in.Reset {
		ok, err := s.start(ctx, hr, in.Parent)
		return restoreResp{Reset: ok}, err
	}
	// The key stays locked until the restore is done: an eviction (or another save of the key) that dropped the
	// snapshot while it is being cloned would leave the shard without a database.
	defer s.lock(hr.Suite, in.Key)()
	c, err := s.Store.Checkpoint(ctx, hr.Suite, in.Key)
	if errors.Is(err, store.ErrNotFound) {
		return restoreResp{}, notFound("no checkpoint for this key")
	}
	if err != nil {
		return restoreResp{}, err
	}
	if hr.Isolation != "isolated" && c.Database != "" {
		return restoreResp{}, notFound("database checkpoints are restored only with isolated shards")
	}
	if why, definite := s.unusable(ctx, hr, c); why != "" {
		if definite {
			s.forget(ctx, hr, c)
		}
		return restoreResp{}, notFound(why)
	}
	path, err := s.file(hr.Suite, in.Key)
	if err != nil {
		return restoreResp{}, err
	}
	p, err := readFile(path)
	if err != nil {
		return restoreResp{}, err
	}
	if c.Database != "" {
		if err := s.recreate(ctx, hr, c.Database); err != nil {
			return restoreResp{}, err
		}
	}
	_ = s.Store.UseCheckpoint(ctx, hr.Suite, in.Key)
	hr.Emit("checkpoint.restored", map[string]any{"flow": c.Flow, "key": in.Key[:16], "db": c.Database != ""})
	return restoreResp{Outputs: p.Outputs, StorageState: p.StorageState}, nil
}

// recreate drops the test's shard database and makes it again as a copy of from. When the copy fails the shard
// gets a clean copy of the template instead, so the stack never keeps running against a database that is gone.
func (s *Service) recreate(ctx context.Context, hr suite.HookRun, from string) error {
	name := hr.Test.DB
	if name == "" || hr.DB == nil {
		return notFound("this test has no shard database to restore")
	}
	if err := hr.DB.Drop(ctx, name); err != nil {
		return fmt.Errorf("drop shard database: %w", err)
	}
	if err := hr.DB.Clone(ctx, from, name); err != nil {
		if hr.Template != "" {
			_ = hr.DB.Clone(context.WithoutCancel(ctx), hr.Template, name)
		}
		return fmt.Errorf("restore shard database: %w", err)
	}
	return nil
}

// start puts an isolated shard's database where a flow starts: the snapshot of the flow before it in the test
// (parent), or the template for the first flow. It says false and touches nothing when that state is not at hand
// (the parent saved no database, or the database is shared): a snapshot taken then would hold other tests' rows.
func (s *Service) start(ctx context.Context, hr suite.HookRun, parent string) (bool, error) {
	if hr.DB == nil || hr.Test.DB == "" || hr.Isolation != "isolated" {
		return false, nil
	}
	from := hr.Template
	if parent != "" {
		from = ""
		if ValidKey(parent) {
			defer s.lock(hr.Suite, parent)() // the parent's snapshot is cloned below: no eviction meanwhile
			if c, err := s.Store.Checkpoint(ctx, hr.Suite, parent); err == nil && c.Database != "" && usable(s.unusable(ctx, hr, c)) {
				from = c.Database
			}
		}
	}
	if from == "" {
		return false, nil
	}
	return true, s.recreate(ctx, hr, from)
}

// maxSize is the suite's suites.<name>.checkpoints.max_size.
func (s *Service) maxSize(hr suite.HookRun) int64 { return maxSizeOf(hr.Config) }

func usable(why string, _ bool) bool { return why == "" }

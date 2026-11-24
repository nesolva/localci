package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nesolva/localci/internal/shard"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
	"gopkg.in/yaml.v3"
)

// fakeDB keeps databases as named strings. It also says which are complete and how big they are, like Postgres.
type fakeDB struct {
	mu       sync.Mutex
	dbs      map[string]string
	drops    []string
	checkErr error             // Complete fails with this, like a database server that cannot be reached
	failing  error             // Snapshot leaves this behind, half copied, and fails
	onClone  func(from string) // called (unlocked) at the start of every Clone: a test stops a restore mid-copy here
}

func newFakeDB() *fakeDB { return &fakeDB{dbs: map[string]string{}} }

func (f *fakeDB) Template(context.Context, string, shard.Seed) (string, error) {
	return "localci_tpl_x", nil
}
func (f *fakeDB) Clone(_ context.Context, from, name string) error {
	if f.onClone != nil {
		f.onClone(from)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.dbs[from]
	if !ok {
		return errors.New("no such database " + from)
	}
	f.dbs[name] = v
	return nil
}
func (f *fakeDB) Drop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(name, "localci_") {
		return errors.New("refusing to drop " + name)
	}
	delete(f.dbs, name)
	f.drops = append(f.drops, name)
	return nil
}
func (f *fakeDB) Snapshot(_ context.Context, from, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		f.dbs[name] = "half" // what a kill mid-copy leaves under the name
		return f.failing
	}
	f.dbs[name] = f.dbs[from]
	return nil
}
func (f *fakeDB) Complete(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkErr != nil {
		return false, f.checkErr
	}
	v, ok := f.dbs[name]
	return ok && v != "half", nil
}
func (f *fakeDB) Size(_ context.Context, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.dbs[name])), nil
}
func (f *fakeDB) set(name, v string) {
	f.mu.Lock()
	f.dbs[name] = v
	f.mu.Unlock()
}
func (f *fakeDB) get(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.dbs[name]
	return v, ok
}

const shardDB = "localci_shop_1_aaaa_1"

type rig struct {
	t      *testing.T
	s      *Service
	st     *store.Store
	db     *fakeDB
	hr     suite.HookRun
	events []string
}

func newRig(t *testing.T, maxSize string) *rig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &rig{t: t, st: st, db: newFakeDB(), s: &Service{Store: st, DataDir: t.TempDir()}}
	r.db.set("localci_tpl_x", "template")
	r.db.set(shardDB, "template")
	var cfg yaml.Node
	doc := "isolation: isolated\n"
	if maxSize != "" {
		doc += "checkpoints:\n  max_size: " + maxSize + "\n"
	}
	if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	r.hr = suite.HookRun{Run: "1", Suite: "shop", Isolation: "isolated", Build: "build1", Template: "localci_tpl_x",
		Test: suite.TestCtx{Test: "t1", DB: shardDB}, DB: r.db, Config: cfg.Content[0],
		Emit: func(kind string, _ any) { r.events = append(r.events, kind) }}
	return r
}

func (r *rig) call(action string, body any) (int, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/hooks/flow/"+action+"?test=t1", bytes.NewReader(b))
	w := httptest.NewRecorder()
	r.s.Hook(w, req, r.hr, action)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

type look struct {
	Flow    string `json:"flow"`
	Source  string `json:"source"`
	Inputs  string `json:"inputs"`
	Parent  string `json:"parent"`
	Browser bool   `json:"browser"`
	DB      bool   `json:"db"`
}

func loginLook() look {
	return look{Flow: "login", Source: "s1", Inputs: "i1", Browser: true, DB: true}
}

func (r *rig) lookup(l look) (key string, hit, save bool, reason string) {
	r.t.Helper()
	code, out := r.call("lookup", l)
	if code != 200 {
		r.t.Fatalf("lookup: %d %v", code, out)
	}
	key, _ = out["key"].(string)
	hit, _ = out["hit"].(bool)
	save, _ = out["save"].(bool)
	reason, _ = out["reason"].(string)
	return
}

func (r *rig) save(key string, l look, outputs string, extra map[string]any) map[string]any {
	r.t.Helper()
	body := map[string]any{"key": key, "flow": l.Flow, "source": l.Source, "inputs": l.Inputs, "parent": l.Parent, "browser": l.Browser, "db": l.DB,
		"storageState": json.RawMessage(`{"cookies":[{"name":"sid","value":"secret-cookie"}]}`), "outputs": json.RawMessage(outputs)}
	for k, v := range extra {
		body[k] = v
	}
	code, out := r.call("save", body)
	if code != 200 {
		r.t.Fatalf("save: %d %v", code, out)
	}
	return out
}

func TestSavedCheckpointIsRestoredOnlyForTheSameFlowInputsCommitAndTemplate(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, hit, save, _ := r.lookup(l)
	if hit || !save {
		t.Fatalf("first lookup: hit=%v save=%v, want a miss that asks for a save", hit, save)
	}
	r.db.set(shardDB, "logged-in")
	if out := r.save(key, l, `{"user":"demo"}`, nil); out["saved"] != true {
		t.Fatalf("save: %v", out)
	}
	if k, hit, _, _ := r.lookup(l); !hit || k != key {
		t.Fatalf("same flow, inputs and commit must hit the saved checkpoint: key %s hit %v", k, hit)
	}

	changed := map[string]func() (look, suite.HookRun){
		"source":   func() (look, suite.HookRun) { x := l; x.Source = "s2"; return x, r.hr },
		"inputs":   func() (look, suite.HookRun) { x := l; x.Inputs = "i2"; return x, r.hr },
		"parent":   func() (look, suite.HookRun) { x := l; x.Parent = strings.Repeat("a", 64); return x, r.hr },
		"commit":   func() (look, suite.HookRun) { h := r.hr; h.Build = "build2"; return l, h },
		"template": func() (look, suite.HookRun) { h := r.hr; h.Template = "localci_tpl_y"; return l, h },
	}
	r.db.set("localci_tpl_y", "template y")
	orig := r.hr
	for what, mk := range changed {
		x, h := mk()
		r.hr = h
		k, hit, _, _ := r.lookup(x)
		r.hr = orig
		if hit || k == key {
			t.Errorf("a new %s must give a new key and run the flow: key same=%v hit=%v", what, k == key, hit)
		}
	}
}

func TestDatabaseCheckpointRunsInsteadOfRestoringUnlessShardsAreIsolated(t *testing.T) {
	for _, level := range []string{"shared-db", "tenant", "serial"} {
		t.Run(level, func(t *testing.T) {
			r := newRig(t, "")
			r.hr.Isolation = "isolated"
			l := loginLook()
			key, _, _, _ := r.lookup(l)
			r.save(key, l, `{}`, nil) // saved while isolated

			r.hr.Isolation = level
			_, hit, save, reason := r.lookup(l)
			if hit || save || !strings.Contains(reason, level) {
				t.Errorf("under %s a database flow must run and say why: hit=%v save=%v reason=%q", level, hit, save, reason)
			}
			if out := r.save(key, l, `{}`, nil); out["saved"] == true {
				t.Errorf("under %s nothing may be saved for a database flow: %v", level, out)
			}
			if code, _ := r.call("restore", map[string]any{"key": key}); code != http.StatusNotFound {
				t.Errorf("restore of a database checkpoint under %s answered %d, want 404", level, code)
			}

			// A browser-only flow still restores.
			b := look{Flow: "cookies", Source: "s", Inputs: "i", Browser: true}
			bk, _, _, _ := r.lookup(b)
			r.save(bk, b, `{}`, nil)
			if _, hit, _, _ := r.lookup(b); !hit {
				t.Errorf("a browser-only checkpoint must restore under %s", level)
			}
		})
	}
}

func TestRestoreRecreatesTheShardDatabaseFromTheSnapshot(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.db.set(shardDB, "after-login")
	r.save(key, l, `{"user":"demo"}`, nil)
	r.db.set(shardDB, "after-the-test-wrote-more")

	code, out := r.call("restore", map[string]any{"key": key})
	if code != 200 {
		t.Fatalf("restore: %d %v", code, out)
	}
	if v, _ := r.db.get(shardDB); v != "after-login" {
		t.Errorf("shard database = %q, want the snapshot's content", v)
	}
	if !strings.Contains(string(mustJSON(out["outputs"])), "demo") || out["storageState"] == nil {
		t.Errorf("restore answered %v, want outputs and storage state", out)
	}
	if !slices.Contains(r.events, "checkpoint.restored") {
		t.Errorf("no checkpoint.restored event: %v", r.events)
	}
}

func TestSnapshotThatDiesMidCopyLeavesNothingRestorable(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.db.failing = errors.New("killed")
	code, _ := r.call("save", map[string]any{"key": key, "flow": "login", "db": true, "browser": true, "outputs": json.RawMessage(`{}`)})
	if code == 200 {
		t.Fatal("a failed snapshot must fail the save")
	}
	if _, ok := r.db.get(CheckpointDB(key)); ok {
		t.Error("the half copy is still there")
	}
	if _, err := r.st.Checkpoint(context.Background(), "shop", key); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a checkpoint row exists for a failed snapshot: %v", err)
	}
	r.db.failing = nil
	if _, hit, _, _ := r.lookup(l); hit {
		t.Error("lookup hit a checkpoint that was never finished")
	}
}

func TestCheckpointWhoseDatabaseOrFilesVanishedIsForgottenNotHalfRestored(t *testing.T) {
	for _, what := range []string{"database", "file"} {
		t.Run(what, func(t *testing.T) {
			r := newRig(t, "")
			l := loginLook()
			key, _, _, _ := r.lookup(l)
			r.save(key, l, `{}`, nil)
			if what == "database" {
				r.db.mu.Lock()
				delete(r.db.dbs, CheckpointDB(key))
				r.db.mu.Unlock()
			} else {
				p, _ := r.s.file("shop", key)
				os.Remove(p)
			}
			if _, hit, save, reason := r.lookup(l); hit || !save || reason == "" {
				t.Errorf("hit=%v save=%v reason=%q, want a miss with a reason", hit, save, reason)
			}
			if _, err := r.st.Checkpoint(context.Background(), "shop", key); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the unusable checkpoint was kept: %v", err)
			}
		})
	}
}

func TestFailedVerifyReplacesTheCheckpoint(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.db.set(shardDB, "v1")
	r.save(key, l, `{"user":"old"}`, nil)

	// Saving again without replace keeps the first (another shard won the race).
	r.db.set(shardDB, "v2")
	r.save(key, l, `{"user":"new"}`, nil)
	_, out := r.call("restore", map[string]any{"key": key})
	if !strings.Contains(string(mustJSON(out["outputs"])), "old") {
		t.Fatalf("a second save without replace changed the checkpoint: %v", out)
	}
	// After a failed verify the package saves with replace and the new state wins.
	r.db.set(shardDB, "v2")
	r.save(key, l, `{"user":"new"}`, map[string]any{"replace": true})
	_, out = r.call("restore", map[string]any{"key": key})
	if !strings.Contains(string(mustJSON(out["outputs"])), "new") {
		t.Errorf("replace did not replace: %v", out)
	}
	if v, _ := r.db.get(shardDB); v != "v2" {
		t.Errorf("shard database after restoring the replaced checkpoint = %q, want v2", v)
	}
}

func TestResetPutsTheShardDatabaseBackToTheParentOrTheTemplate(t *testing.T) {
	r := newRig(t, "")
	parentLook := look{Flow: "create-shop", Source: "s", Inputs: "i", DB: true}
	pk, _, _, _ := r.lookup(parentLook)
	r.db.set(shardDB, "shop-made")
	r.save(pk, parentLook, `{}`, nil)

	r.db.set(shardDB, "half-logged-in")
	if code, out := r.call("restore", map[string]any{"key": strings.Repeat("b", 64), "reset": true, "parent": pk}); code != 200 {
		t.Fatalf("reset: %d %v", code, out)
	}
	if v, _ := r.db.get(shardDB); v != "shop-made" {
		t.Errorf("reset with a parent: shard database %q, want the parent's snapshot", v)
	}
	r.db.set(shardDB, "garbage")
	if code, _ := r.call("restore", map[string]any{"key": strings.Repeat("b", 64), "reset": true}); code != 200 {
		t.Fatal("reset without a parent failed")
	}
	if v, _ := r.db.get(shardDB); v != "template" {
		t.Errorf("reset without a parent: shard database %q, want the template", v)
	}
}

func TestEvictionKeepsTheStoreUnderTheCapLeastRecentlyUsedFirstAndDropsTheirDatabases(t *testing.T) {
	r := newRig(t, "3500")
	ctx := context.Background()
	big := strings.Repeat("x", 1000)
	keys := map[string]string{}
	for i, name := range []string{"a", "b", "c"} {
		l := look{Flow: name, Source: "s", Inputs: "i", DB: true}
		k, _, _, _ := r.lookup(l)
		r.db.set(shardDB, big)
		r.save(k, l, `{}`, nil)
		keys[name] = k
		// Give every checkpoint a distinct, increasing last use.
		c, _ := r.st.Checkpoint(ctx, "shop", k)
		c.Used = int64(1000 + i)
		_ = r.st.PutCheckpoint(ctx, c)
	}
	// "a" is used again, so "b" is now the least recently used.
	if _, out := r.call("restore", map[string]any{"key": keys["a"]}); out["error"] != nil {
		t.Fatal(out)
	}
	c, _ := r.st.Checkpoint(ctx, "shop", keys["a"])
	c.Used = 5000
	_ = r.st.PutCheckpoint(ctx, c)

	l := look{Flow: "d", Source: "s", Inputs: "i", DB: true}
	k, _, _, _ := r.lookup(l)
	r.db.set(shardDB, big)
	r.save(k, l, `{}`, nil)

	left, _ := r.st.Checkpoints(ctx, "shop")
	var total int64
	for _, c := range left {
		total += c.Bytes
	}
	if total > 3500 {
		t.Errorf("the store holds %d bytes, over the 3500 cap", total)
	}
	have := map[string]bool{}
	for _, c := range left {
		have[c.Flow] = true
	}
	if have["b"] || !have["a"] || !have["d"] {
		t.Errorf("kept %v, want the least recently used (b) evicted and a, d kept", have)
	}
	if _, ok := r.db.get(CheckpointDB(keys["b"])); ok {
		t.Error("the evicted checkpoint's database is still there")
	}
	if !slices.Contains(r.db.drops, CheckpointDB(keys["b"])) {
		t.Errorf("drops = %v, want the evicted database dropped", r.db.drops)
	}
	if !slices.Contains(r.events, "checkpoint.evicted") {
		t.Error("no checkpoint.evicted event")
	}
}

func TestCheckpointLargerThanTheCapIsNotSaved(t *testing.T) {
	r := newRig(t, "100")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.db.set(shardDB, strings.Repeat("x", 500))
	out := r.save(key, l, `{}`, nil)
	if out["saved"] == true || out["reason"] == nil {
		t.Errorf("%v: a checkpoint over the cap must not be saved, and says why", out)
	}
	if _, ok := r.db.get(CheckpointDB(key)); ok {
		t.Error("its snapshot database was left behind")
	}
}

func TestCheckpointFilesAreOwnerOnlyBecauseTheyHoldSessionCookies(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.save(key, l, `{}`, nil)
	p, _ := r.s.file("shop", key)
	for path, want := range map[string]os.FileMode{filepath.Dir(p): 0o700, p: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", path, fi, err, want)
		}
	}
}

func TestSuiteNamesThatCouldEscapeTheCheckpointDirectoryAreRefused(t *testing.T) {
	r := newRig(t, "")
	r.hr.Suite = "../evil"
	code, _ := r.call("save", map[string]any{"key": strings.Repeat("a", 64), "flow": "x", "outputs": json.RawMessage(`{}`), "browser": true})
	if code == 200 {
		t.Error("a suite name with .. named a directory")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// A restored browser-only checkpoint that fails verify asks for a reset; under a shared database that database holds
// every shard's data, so a reset must leave it alone.
func TestResetNeverTouchesADatabaseOtherShardsShare(t *testing.T) {
	for _, level := range []string{"shared-db", "tenant", "auto", "serial"} {
		t.Run(level, func(t *testing.T) {
			r := newRig(t, "")
			r.hr.Isolation = level
			r.db.set(shardDB, "every shard's rows")
			if code, out := r.call("restore", map[string]any{"key": strings.Repeat("b", 64), "reset": true}); code != 200 {
				t.Fatalf("reset: %d %v", code, out)
			}
			if v, _ := r.db.get(shardDB); v != "every shard's rows" || len(r.db.drops) > 0 {
				t.Errorf("reset under %s replaced the shared database (now %q, drops %v)", level, v, r.db.drops)
			}
		})
	}
}

// The key a save writes under is bound to the commits under test: a run of other commits (a branch, a fork) cannot
// plant a checkpoint that a run of these commits then restores.
func TestSaveCannotWriteUnderAKeyOfOtherCommits(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	r.hr.Build = "main"
	mainKey, _, _, _ := r.lookup(l)

	r.hr.Build = "branch"
	r.db.set(shardDB, "branch state")
	if code, out := r.call("save", map[string]any{"key": mainKey, "flow": l.Flow, "source": l.Source, "inputs": l.Inputs,
		"browser": l.Browser, "db": l.DB, "outputs": json.RawMessage(`{}`)}); code == 200 && out["saved"] == true {
		t.Errorf("a run of other commits saved under main's key: %d %v", code, out)
	}

	r.hr.Build = "main"
	if _, hit, _, _ := r.lookup(l); hit {
		t.Error("main restores a checkpoint another build saved")
	}
}

// An isolated shard's database holds whatever earlier tests on that shard wrote. A database checkpoint must hold
// only what the flow wrote on top of where it starts (the template, or the flow before it), or a restore elsewhere
// hands a test another test's rows.
func TestDatabaseCheckpointHoldsOnlyWhatItsFlowWrote(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	r.db.set(shardDB, "template+earlier test's rows")
	key, hit, save, _ := r.lookup(l)
	if hit || !save {
		t.Fatalf("first lookup: hit=%v save=%v", hit, save)
	}
	v, _ := r.db.get(shardDB)
	r.db.set(shardDB, v+"+login") // the flow runs
	r.save(key, l, `{}`, nil)

	r.db.set(shardDB, "template+another test's rows")
	if _, hit, _, _ := r.lookup(l); !hit {
		t.Fatal("second lookup missed")
	}
	if code, out := r.call("restore", map[string]any{"key": key}); code != 200 {
		t.Fatalf("restore: %d %v", code, out)
	}
	if v, _ := r.db.get(shardDB); v != "template+login" {
		t.Errorf("restored database = %q, want only the template and the flow's own writes", v)
	}

	// After a flow whose database state was not saved, there is no clean start to snapshot from: run, don't save.
	r.db.set(shardDB, "template+rows")
	after := look{Flow: "add-to-cart", Source: "s", Inputs: "i", Parent: strings.Repeat("c", 64), DB: true}
	if _, _, save, reason := r.lookup(after); save || reason == "" {
		t.Errorf("a database flow after an unsaved one must run without saving and say why: save=%v reason=%q", save, reason)
	}
	if v, _ := r.db.get(shardDB); v != "template+rows" {
		t.Errorf("that lookup changed the shard database to %q", v)
	}
}

// A fork run's flows run but leave no checkpoint behind: a checkpoint is state later runs restore.
func TestForkRunSavesNoCheckpoint(t *testing.T) {
	r := newRig(t, "")
	r.hr.Fork = true
	l := loginLook()
	key, hit, save, reason := r.lookup(l)
	if hit || save || reason == "" {
		t.Errorf("fork lookup: hit=%v save=%v reason=%q, want a run that is not saved and says why", hit, save, reason)
	}
	if out := r.save(key, l, `{}`, nil); out["saved"] == true {
		t.Errorf("a fork run saved a checkpoint: %v", out)
	}
}

// The snapshot database is named by the key alone, so two suites that share a flow, its inputs and the build must
// not share a key, or a checkpoint of one is restored (and evicted) by the other.
func TestSuitesWithTheSameFlowGetDifferentKeysAndDatabases(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	shopKey, _, _, _ := r.lookup(l)
	r.hr.Suite = "blog"
	blogKey, hit, _, _ := r.lookup(l)
	if shopKey == blogKey || CheckpointDB(shopKey) == CheckpointDB(blogKey) {
		t.Fatalf("shop and blog share a key or a database: %s %s", shopKey, blogKey)
	}
	if hit {
		t.Error("blog hit a checkpoint of shop")
	}
}

// Eviction must not drop a snapshot while a test is cloning it. The restore is stopped in the middle of the copy,
// a save then needs room (the restored checkpoint is the least recently used), and only afterwards may it go.
func TestEvictionSkipsACheckpointThatIsBeingRestoredAndTakesItOnceItIsFree(t *testing.T) {
	r := newRig(t, "3500")
	ctx := context.Background()
	big := strings.Repeat("x", 1000)
	keys := map[string]string{}
	save := func(name string, used int64) {
		l := look{Flow: name, Source: "s", Inputs: "i", DB: true}
		k, _, _, _ := r.lookup(l)
		r.db.set(shardDB, big)
		r.save(k, l, `{}`, nil)
		keys[name] = k
		c, _ := r.st.Checkpoint(ctx, "shop", k)
		c.Used = used
		_ = r.st.PutCheckpoint(ctx, c)
	}
	save("a", 1000)
	save("b", 1001)
	save("c", 1002)

	other := r.hr // a second shard, so the save below does not touch the database being restored
	other.Test.DB = "localci_shop_1_aaaa_2"
	r.db.set(other.Test.DB, "template")
	cloning, release := make(chan struct{}), make(chan struct{})
	r.db.onClone = func(from string) {
		if from == CheckpointDB(keys["a"]) {
			close(cloning)
			<-release
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.s.restore(ctx, other, restoreReq{Key: keys["a"]})
		done <- err
	}()
	<-cloning

	save("d", 2000) // over the cap: "a" is the least recently used, and in use
	if _, ok := r.db.get(CheckpointDB(keys["a"])); !ok {
		t.Fatal("the snapshot was dropped while a test was cloning it")
	}
	if _, err := r.st.Checkpoint(ctx, "shop", keys["a"]); err != nil {
		t.Errorf("the checkpoint in use was forgotten: %v", err)
	}
	if _, err := r.st.Checkpoint(ctx, "shop", keys["b"]); err == nil {
		t.Error("nothing was evicted for the save: the next least recently used (b) should have gone")
	}

	r.db.onClone = nil
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("restore: %v", err)
	}
	if v, _ := r.db.get(other.Test.DB); v != big {
		t.Errorf("the shard database holds %q after the restore, want the snapshot", v)
	}

	// Free again and the oldest in use: the next save that needs room takes it.
	c, _ := r.st.Checkpoint(ctx, "shop", keys["a"])
	c.Used = 1
	_ = r.st.PutCheckpoint(ctx, c)
	save("e", 3000)
	if _, err := r.st.Checkpoint(ctx, "shop", keys["a"]); err == nil {
		t.Error("the checkpoint stayed after its restore finished")
	}
}

func TestALookupThatCannotCheckACheckpointLeavesItForTheOthersUsingIt(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.save(key, l, `{}`, nil)

	r.db.mu.Lock()
	r.db.checkErr = errors.New("connection refused")
	r.db.mu.Unlock()
	if _, hit, _, reason := r.lookup(l); hit || reason == "" {
		t.Fatalf("hit=%v reason=%q, want a miss with a reason", hit, reason)
	}
	if _, ok := r.db.get(CheckpointDB(key)); !ok {
		t.Error("the snapshot was dropped because a check failed, not because it was found unusable")
	}
	if _, err := r.st.Checkpoint(context.Background(), "shop", key); err != nil {
		t.Errorf("the checkpoint was forgotten on a transient check error: %v", err)
	}
}

func TestALookupDoesNotForgetAnUnusableCheckpointWhileTheKeyIsInUse(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.save(key, l, `{}`, nil)
	p, _ := r.s.file("shop", key)
	os.Remove(p)

	unlock := r.s.lock("shop", key) // a restore or a save holds the key
	if _, hit, _, _ := r.lookup(l); hit {
		t.Fatal("hit on a checkpoint whose files are gone")
	}
	if _, ok := r.db.get(CheckpointDB(key)); !ok {
		t.Error("the snapshot was dropped under a key another shard holds")
	}
	unlock()
	r.lookup(l)
	if _, err := r.st.Checkpoint(context.Background(), "shop", key); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the unusable checkpoint was not forgotten once the key was free: %v", err)
	}
}

// A run whose stack or tenant flow broke saved nothing that later runs would restore, but it still restores what
// healthy runs saved.
func TestInfrastructureFailureRunSavesNoCheckpointButStillRestores(t *testing.T) {
	r := newRig(t, "")
	l := loginLook()
	key, _, _, _ := r.lookup(l)
	r.save(key, l, `{}`, nil) // a healthy run saved it

	r.hr.NoSave = true
	if _, hit, save, _ := r.lookup(l); !hit || save {
		t.Errorf("lookup of a saved flow: hit=%v save=%v, want a hit that is not saved again", hit, save)
	}
	other := look{Flow: "other", Source: "s", Inputs: "i", Browser: true}
	k2, hit, save, _ := r.lookup(other)
	if hit || save {
		t.Errorf("a miss in an infrastructure run: hit=%v save=%v, want no save", hit, save)
	}
	if out := r.save(k2, other, `{}`, nil); out["saved"] == true {
		t.Errorf("an infrastructure run saved a checkpoint: %v", out)
	}
	if _, err := r.st.Checkpoint(context.Background(), "shop", k2); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the checkpoint was stored: %v", err)
	}
}

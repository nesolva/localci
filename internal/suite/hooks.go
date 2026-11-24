package suite

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"

	"github.com/nesolva/localci/internal/shard"
	"gopkg.in/yaml.v3"
)

// TestCtx is what code hooked around every test gets to know about it.
type TestCtx struct {
	Run   string
	Suite string
	Shard int // 0 for the serial group and the reruns
	Test  string
	DB    string         // the shard's database name, "" when the suite has none
	Ports map[string]int // the shard's named ports so far
	Dirs  map[string]string
	// Out is where the shard's invocation writes artifacts ($LOCALCI_OUTPUT_DIR); hooks put their dumps there,
	// never in the workspace. Empty before the first invocation of the shard.
	Out string
	// Env is what every process of the shard gets (LOCALCI_RUN, LOCALCI_SHARD, the database).
	Env []string
	// Expand resolves the placeholders of a suite command ({{port "api"}}, {{db.url}}, {{dir "repo"}}) for the
	// shard the test runs on; nil when the test is not known to a shard.
	Expand func(string) (string, error)
	// Remote: the test runs on a worker, so there is no shard environment here (no database, ports or directories of its own).
	Remote bool
}

type hook struct{ begin, end func(TestCtx) }

// HookRun is what a hook area (flows, …) learns about the run a call belongs to.
type HookRun struct {
	Run       string
	Suite     string
	Isolation string // the suite's isolation level
	// Fork: the heads include a pull request head from another repository; the run may use what trusted runs
	// learned but leaves nothing behind for them.
	Fork bool
	// NoSave: the run is an infrastructure failure (a service died, the tenant flow failed). What it measured is the
	// breakage, so it may still restore what earlier runs saved but must save no checkpoint of its own.
	NoSave bool
	Test   TestCtx
	// Remote: the test runs on a worker; DB is nil and nothing local (databases, checkpoints of a database) applies.
	Remote bool
	// Build identifies the commits under test: a hash of every repo's tested commit (the stash commit for a working
	// tree), never a branch name.
	Build string
	// Template is the template database the shards were cloned from, "" when the suite has no database.
	Template string
	// DB makes and drops the run's databases; nil when the suite has none.
	DB      shard.DB
	DataDir string
	// Config is the suite's own mapping, for the settings an area reads (suites.<name>.<area>).
	Config *yaml.Node
	// Emit adds a core event to the run's log.
	Emit func(kind string, data any)
}

// HookFunc answers one call of a hook area: POST /runs/{run}/hooks/<area>/<action>?test=<id>.
type HookFunc func(w http.ResponseWriter, req *http.Request, hr HookRun, action string)

// HandleHook registers the handler of a hook area other than test. Register before runs start.
func (r *Runner) HandleHook(area string, h HookFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.areas == nil {
		r.areas = map[string]HookFunc{}
	}
	r.areas[area] = h
}

func (ex *execution) hookRun(test string) HookRun {
	hr := HookRun{Run: ex.id, Suite: ex.suite.Name, Isolation: ex.suite.Isolation, Fork: ex.fork(), NoSave: ex.infraReason() != "", Test: ex.testCtx(test),
		DataDir: ex.r.d.DataDir, Config: ex.suite.Raw,
		Emit: func(kind string, data any) { ex.emit(0, kind, test, "", data) }}
	ex.mu.Lock()
	var parts []string
	for repo, sha := range ex.shas {
		parts = append(parts, repo+"@"+sha)
	}
	ex.mu.Unlock()
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	hr.Build = hex.EncodeToString(sum[:])[:16]
	if ex.db != nil && !hr.Test.Remote {
		hr.DB, hr.Template = ex.db.drv, ex.db.tpl
	}
	hr.Remote = hr.Test.Remote
	return hr
}

// OnTest registers code to run around every test of every suite: begin before the test starts, end after it
// finishes. Either may be nil. The test waits for the hook (the Playwright fixture calls the hooks endpoint and
// blocks), so hooks stay short. Register before runs start.
func (r *Runner) OnTest(begin, end func(TestCtx)) {
	r.mu.Lock()
	r.hooks = append(r.hooks, hook{begin, end})
	r.mu.Unlock()
}

// Handler serves the hooks the test package calls: POST /runs/{run}/hooks/test/begin?test=<id> and .../end. A
// shard's $LOCALCI_API is the base URL it is served at plus /runs/{run}, so mounting it under a prefix means
// setting Deps.API to the same prefix.
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /runs/{run}/hooks/test/{kind}", func(w http.ResponseWriter, req *http.Request) {
		kind, test := req.PathValue("kind"), req.URL.Query().Get("test")
		if (kind != "begin" && kind != "end") || test == "" {
			http.Error(w, "use /hooks/test/begin or end with ?test=<id>", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		ex := r.live[req.PathValue("run")]
		hooks := append([]hook(nil), r.hooks...)
		r.mu.Unlock()
		if ex == nil {
			http.Error(w, "no such run in progress", http.StatusNotFound)
			return
		}
		tc := ex.testCtx(test)
		if tc.Remote { // what the hooks do (isolation, coverage) needs the test's shard here; a worker's tests have none
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for _, h := range hooks {
			if fn := map[string]func(TestCtx){"begin": h.begin, "end": h.end}[kind]; fn != nil {
				fn(tc)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /runs/{run}/hooks/{area}/{action}", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		h := r.areas[req.PathValue("area")]
		ex := r.live[req.PathValue("run")]
		r.mu.Unlock()
		switch {
		case h == nil:
			http.Error(w, "no such hook area", http.StatusNotFound)
		case ex == nil:
			http.Error(w, "no such run in progress", http.StatusNotFound)
		default:
			h(w, req, ex.hookRun(req.URL.Query().Get("test")), req.PathValue("action"))
		}
	})
	return mux
}

func (ex *execution) testCtx(test string) TestCtx {
	tc := TestCtx{Run: ex.id, Suite: ex.suite.Name, Test: test, Dirs: map[string]string{}}
	ex.mu.Lock()
	e := ex.envOf[test]
	tc.Remote = ex.remote[test]
	for k, v := range ex.dirs {
		tc.Dirs[k] = v
	}
	ex.mu.Unlock()
	if e != nil {
		tc.Shard, tc.DB = e.n, e.dbName
		tc.Env, tc.Expand = e.coreEnv(), e.templ().expand
		e.mu.Lock()
		tc.Out = e.out
		tc.Ports = map[string]int{}
		for k, v := range e.ports {
			tc.Ports[k] = v
		}
		e.mu.Unlock()
	}
	return tc
}

func newHookToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("localci: no randomness for a hook token: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
}

// HookTokenValid reports whether token is the hook token of a run in progress. Every hook call must carry its own
// run's token: the shards of one run get it as $LOCALCI_HOOK_TOKEN and nothing else holds it, so a process that
// merely reaches the port cannot drive a run's hooks. It is refused once the run is over.
func (r *Runner) HookTokenValid(run, token string) bool {
	r.mu.Lock()
	ex := r.live[run]
	r.mu.Unlock()
	return ex != nil && token != "" && subtle.ConstantTimeCompare([]byte(ex.hookToken), []byte(token)) == 1
}

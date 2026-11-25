package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs", s.submit)
	mux.HandleFunc("GET /api/runs", s.runs)
	mux.HandleFunc("GET /api/runs/{id}", s.run)
	mux.HandleFunc("GET /api/runs/{id}/failures", s.failures)
	mux.HandleFunc("GET /api/runs/{id}/affected", s.affected)
	mux.HandleFunc("GET /api/runs/{id}/log", s.log)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/runs/{id}/retry", s.retry)
	mux.Handle("GET /api/events", event.SSE(s.d.Store, s.d.Bus))
	mux.HandleFunc("GET /api/queue", s.queue)
	mux.HandleFunc("GET /api/workers", s.workers)
	mux.HandleFunc("POST /api/workers/token", s.workerToken)
	mux.HandleFunc("POST /api/workers/{name}/revoke", s.workerRevoke)
	s.triageRoutes(mux)
	s.exploreRoutes(mux)
	mux.HandleFunc("GET /api/config", s.config)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, http.StatusNotFound, "no such endpoint") })
}

func (s *Server) lastSeq(ctx context.Context) int64 {
	n, _ := s.d.Store.LastSeq(ctx)
	return n
}

// summarize turns a stored run into what the API returns.
func (s *Server) summarize(ctx context.Context, r store.Run) Run {
	out := Run{ID: r.ID, Suite: r.Suite, Kind: r.Kind, State: r.State, Reason: r.Reason, Infra: r.Infra, Trigger: r.Trigger,
		Created: r.Created, Started: r.Started, Finished: r.Finished, Affected: r.Affected}
	if len(r.Heads) > 0 {
		json.Unmarshal(r.Heads, &out.Heads)
	}
	if c, err := s.d.Store.TestCounts(ctx, r.ID); err == nil {
		out.Tests, out.Passed, out.Failed = c.Total, c.Passed, c.Failed
	}
	if ev, ok, _ := s.d.Store.FirstEvent(ctx, r.ID, "run.planned"); ok {
		var p struct{ Tests, Shards int }
		if json.Unmarshal(ev.Data, &p) == nil {
			out.Tests = max(out.Tests, p.Tests)
			out.Shards = p.Shards
		}
	}
	if s.d.Suites != nil && r.Kind == store.KindSuite && !store.Terminal(r.State) {
		if eta, ok, err := s.d.Suites.ETA(ctx, r.ID); err == nil && ok {
			out.ETAMS = eta.Milliseconds()
		}
	}
	return out
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cursor := s.lastSeq(ctx)
	var since int64
	var newer time.Duration
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			since = n
		} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
			newer = d
		} else {
			fail(w, http.StatusBadRequest, "since is an event number or a duration like 30m, got %q", v)
			return
		}
	}
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "limit must be a positive number, got %q", v)
			return
		}
		limit = min(n, maxLimit)
	}
	all, err := s.d.Store.Runs(ctx, since)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if newer > 0 {
		cut := time.Now().Add(-newer).UnixMilli()
		kept := all[:0]
		for _, run := range all {
			if run.Created >= cut || !store.Terminal(run.State) {
				kept = append(kept, run)
			}
		}
		all = kept
	}
	list := RunList{Runs: []Run{}, Total: len(all), Cursor: cursor}
	for _, run := range all[:min(limit, len(all))] {
		list.Runs = append(list.Runs, s.summarize(ctx, run))
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) (store.Run, bool) {
	id := r.PathValue("id")
	run, err := s.d.Store.Run(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		fail(w, http.StatusNotFound, "no run %s", id)
		return run, false
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return run, false
	}
	return run, true
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	d := RunDetail{Run: s.summarize(r.Context(), run)}
	if shards, err := s.d.Store.Shards(r.Context(), run.ID); err == nil {
		for _, sh := range shards {
			d.ShardList = append(d.ShardList, ShardInfo{N: sh.N, State: sh.State, Label: ShardLabel(sh.N), LogPath: sh.LogPath})
		}
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) failures(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cursor := s.lastSeq(ctx)
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, "since is an event number, got %q", v)
			return
		}
		since = n
	}
	tests, err := s.d.Store.Failures(ctx, run.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	ends := map[string]int64{}
	if since > 0 {
		ends, _ = s.d.Store.TestEnds(ctx, run.ID)
	}
	arts, _ := s.d.Store.Artifacts(ctx, run.ID)
	out := FailureList{Run: s.summarize(ctx, run), Total: len(tests), Failures: []Failure{}, Cursor: cursor}
	for _, t := range tests {
		if since > 0 && ends[t.ID] <= since {
			continue
		}
		out.Failures = append(out.Failures, failureOf(t, arts))
	}
	writeJSON(w, http.StatusOK, out)
}

// failureOf flattens a stored test and its failure report.
func failureOf(t store.Test, arts []store.Artifact) Failure {
	f := Failure{Test: t.ID, Title: t.Title, File: t.File, Line: t.Line, Shard: t.Shard, Label: ShardLabel(t.Shard), MS: t.MS, Status: t.Status, Class: t.Class}
	var rep struct {
		Step       string         `json:"step"`
		Message    string         `json:"message"`
		Expected   *string        `json:"expected"`
		Actual     *string        `json:"actual"`
		Location   *string        `json:"location"`
		Aria       *string        `json:"aria"`
		Console    []string       `json:"console"`
		Network    []NetworkError `json:"network"`
		Screenshot *string        `json:"screenshot"`
		Trace      *string        `json:"trace"`
	}
	if len(t.Failure) > 0 && json.Unmarshal(t.Failure, &rep) == nil {
		f.Step, f.Message = rep.Step, rep.Message
		f.Expected, f.Actual = deref(rep.Expected), deref(rep.Actual)
		f.Location, f.Aria = deref(rep.Location), deref(rep.Aria)
		f.Console, f.Network = rep.Console, rep.Network
		f.Screenshot, f.Trace = deref(rep.Screenshot), deref(rep.Trace)
	}
	// An artifact the reporter stored fills what the report did not carry.
	for _, a := range arts {
		if a.Test != t.ID {
			continue
		}
		switch {
		case a.Kind == "screenshot" && f.Screenshot == "":
			f.Screenshot = a.Path
		case a.Kind == "trace" && f.Trace == "":
			f.Trace = a.Path
		}
	}
	return f
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// log serves a shard's log: the last tail lines (default 400), and with follow=1 whatever is appended until the run
// ends. The default shard is the first one with a failure, else the first.
func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	shards, _ := s.d.Store.Shards(ctx, run.ID)
	want := 0
	if v := r.URL.Query().Get("shard"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "shard is a number from 1, got %q", v)
			return
		}
		want = n
	} else if fs, _ := s.d.Store.Failures(ctx, run.ID); len(fs) > 0 && fs[0].Shard > 0 {
		want = fs[0].Shard
	}
	var path string
	for _, sh := range shards {
		if sh.LogPath != "" && (want == 0 || sh.N == want) {
			path = sh.LogPath
			break
		}
	}
	if path == "" {
		fail(w, http.StatusNotFound, "run %s has no log yet", run.ID)
		return
	}
	tail := 400
	if v := r.URL.Query().Get("tail"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tail = min(n, 100000)
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Log-Path", path)
	f, err := os.Open(path)
	if err != nil {
		fail(w, http.StatusNotFound, "log %s is not readable", path)
		return
	}
	defer f.Close()
	lines, offset := tailLines(f, tail)
	for _, l := range lines {
		io.WriteString(w, l+"\n")
	}
	if r.URL.Query().Get("follow") != "1" {
		return
	}
	rc := http.NewResponseController(w)
	rc.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		n, _ := io.Copy(w, io.NewSectionReader(f, offset, 1<<30))
		offset += n
		rc.Flush()
		cur, err := s.d.Store.Run(ctx, run.ID)
		if err != nil || (store.Terminal(cur.State) && n == 0) {
			return
		}
	}
}

const (
	tailWindow = 8 << 20  // the most a tail reads from the end of a log
	tailLine   = 64 << 10 // a longer line is cut, so a client reading the tail line by line never chokes on it
)

// tailLines returns the last n lines of f and the offset after the last byte read, where following resumes. It reads
// backwards from the end, at most tailWindow bytes, so a log of any size or line length costs the same.
func tailLines(f *os.File, n int) ([]string, int64) {
	fi, err := f.Stat()
	if err != nil {
		return nil, 0
	}
	end := fi.Size()
	start, newlines := end, 0
	var chunks [][]byte
	for start > 0 && end-start < tailWindow && newlines <= n { // n lines need n+1 newlines: theirs and the one before
		k := min(64<<10, start, tailWindow-(end-start))
		b := make([]byte, k)
		if _, err := f.ReadAt(b, start-k); err != nil {
			break
		}
		start -= k
		newlines += bytes.Count(b, []byte{'\n'})
		chunks = append(chunks, b)
	}
	slices.Reverse(chunks)
	text := string(bytes.Join(chunks, nil))
	if start > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:] // the first line is cut in the middle
		}
	}
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil, end
	}
	lines := strings.Split(text, "\n")
	lines = lines[max(len(lines)-n, 0):]
	for i, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		if len(l) > tailLine {
			cut := tailLine
			for cut > 0 && !utf8.RuneStart(l[cut]) {
				cut--
			}
			l = l[:cut] + "…"
		}
		lines[i] = l
	}
	return lines, end
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.d.Store.Runs(ctx, 0)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	q := Queue{Runs: []Run{}}
	for i := len(all) - 1; i >= 0; i-- { // oldest first
		if !store.Terminal(all[i].State) {
			q.Runs = append(q.Runs, s.summarize(ctx, all[i]))
		}
	}
	if s.d.Sched != nil {
		q.Waiting = s.d.Sched.Stats().Waiting
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) workers(w http.ResponseWriter, r *http.Request) {
	me := Worker{Name: "this machine", CPUs: s.d.Machine.CPUs, Memory: s.d.Machine.FreeMemory}
	if s.d.Sched != nil {
		st := s.d.Sched.Stats()
		me.CPUs, me.Memory = st.Capacity.CPUs, st.Capacity.Memory
		me.UsedCPUs, me.UsedMemory, me.Running = st.UsedCPUs, st.UsedMemory, st.Running
	}
	writeJSON(w, http.StatusOK, Workers{Workers: append([]Worker{me}, s.remoteWorkers(r.Context())...)})
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	if s.d.Config == nil {
		fail(w, http.StatusNotFound, "no configuration loaded")
		return
	}
	eff, err := s.d.Config()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	b, err := eff.JSON()
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	if store.Terminal(run.State) {
		fail(w, http.StatusConflict, "run %s already %s", run.ID, run.State)
		return
	}
	switch {
	case run.Kind == store.KindSuite && s.d.Suites != nil:
		s.d.Suites.Cancel(run.ID)
	case run.Kind == store.KindCheck && s.d.Checks != nil:
		s.d.Checks.Cancel(run.ID)
	default:
		fail(w, http.StatusConflict, "run %s cannot be cancelled by this server", run.ID)
		return
	}
	if cur, err := s.d.Store.Run(r.Context(), run.ID); err == nil {
		run = cur
	}
	writeJSON(w, http.StatusOK, s.summarize(r.Context(), run))
}

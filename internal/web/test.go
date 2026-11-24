package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nesolva/localci/internal/store"
)

// failure mirrors the report protocol's failure object, tolerant of the shapes a reporter may send.
type failure struct {
	Step       string
	Message    string
	Expected   string
	Actual     string
	HasMatcher bool
	Location   string
	Aria       string
	Console    []string
	Network    []netLine
	Screenshot string
	Trace      string
}

type netLine struct {
	Method string
	URL    string
	Status int
}

func parseFailure(raw json.RawMessage) failure {
	var f failure
	if len(raw) == 0 || string(raw) == "null" {
		return f
	}
	var m struct {
		Step     string            `json:"step"`
		Message  string            `json:"message"`
		Expected json.RawMessage   `json:"expected"`
		Actual   json.RawMessage   `json:"actual"`
		Location string            `json:"location"`
		Aria     json.RawMessage   `json:"aria"`
		Console  []json.RawMessage `json:"console"`
		Network  []struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Status int    `json:"status"`
		} `json:"network"`
		Screenshot string `json:"screenshot"`
		Trace      string `json:"trace"`
	}
	if json.Unmarshal(raw, &m) != nil {
		f.Message = string(raw)
		return f
	}
	f.Step, f.Message, f.Location, f.Screenshot, f.Trace = m.Step, m.Message, m.Location, m.Screenshot, m.Trace
	f.Expected, f.Actual = scalar(m.Expected), scalar(m.Actual)
	f.HasMatcher = f.Expected != "" || f.Actual != ""
	f.Aria = strings.Join(textLines(m.Aria), "\n")
	for _, c := range m.Console {
		f.Console = append(f.Console, textLines(c)...)
	}
	for _, n := range m.Network {
		f.Network = append(f.Network, netLine{Method: n.Method, URL: n.URL, Status: n.Status})
	}
	return f
}

// scalar shows a JSON value as text: strings as they are, everything else as compact JSON; null as empty.
func scalar(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}

// textLines reads a JSON string, a list of strings, or a list of {text|message} objects.
func textLines(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.Split(strings.TrimRight(s, "\n"), "\n")
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var out []string
		for _, e := range list {
			out = append(out, textLines(e)...)
		}
		return out
	}
	var o struct {
		Text    string `json:"text"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil && (o.Text != "" || o.Message != "") {
		return []string{o.Text + o.Message}
	}
	return []string{string(raw)}
}

type testView struct {
	Page
	R        store.Run
	T        store.Test
	F        failure
	Failed   bool
	Steps    []stepRow
	Shot     bool
	TraceCmd string
	Evidence string
	Class    string
	Attempt  attemptNav
}

type stepRow struct {
	Title string
	MS    string
	Class string
	Depth int
}

func (s *server) test(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run, err := s.st.Run(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.st.RunTest(ctx, run.ID, r.PathValue("test"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := testView{Page: Page{Title: t.Title, Run: run.ID, Live: !store.Terminal(run.State)}, R: run, T: t, F: parseFailure(t.Failure),
		Failed: isFailed(t.Status), Class: t.Class}
	v.Seq = s.seq(ctx)
	if p := s.screenshotPath(r, t); p.real != "" {
		v.Shot = true
	}
	if v.F.Trace != "" {
		v.TraceCmd = "npx playwright show-trace " + shellQuote(v.F.Trace)
	}
	if len(t.Evidence) > 0 {
		v.Evidence = scalar(t.Evidence)
	}
	steps, nav, err := s.testSteps(r, run, t.ID, attemptParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.Steps, v.Attempt = steps, nav
	s.render(w, http.StatusOK, "test", v)
}

func (s *server) testSteps(r *http.Request, run store.Run, test string, sel int) ([]stepRow, attemptNav, error) {
	evs, err := s.st.RunEvents(r.Context(), run.ID)
	if err != nil {
		return nil, attemptNav{}, err
	}
	data := parseRunEvents(evs)
	var mine []tstep
	nav := attemptNav{}
	if a := data.pick(test, sel); a != nil && len(a.Steps) > 0 {
		nav = newAttemptNav(r, len(data.attempts[test]), sel)
		active := !store.Terminal(run.State)
		for _, st := range a.Steps {
			c := *st
			c.Open = st.OK == nil && (!active || a != data.last(test))
			c.Offset = st.Start
			mine = append(mine, c)
		}
	} else { // steps stored without events (older runs)
		lanes, err := s.st.Timeline(r.Context(), run.ID)
		if err != nil {
			return nil, nav, err
		}
		for _, l := range lanes {
			for _, st := range l.Steps {
				if st.Test == test {
					mine = append(mine, tstep{TimelineStep: st, Open: st.OK == nil && store.Terminal(run.State)})
				}
			}
		}
	}
	sort.SliceStable(mine, func(i, j int) bool { return mine[i].Offset < mine[j].Offset })
	depth := map[string]int{}
	out := make([]stepRow, 0, len(mine))
	for _, st := range mine {
		d := 0
		if st.Parent != "" {
			d = depth[st.Parent] + 1
		}
		depth[st.ID] = d
		ms := fmtMS(st.MS)
		if st.Open {
			ms = "unfinished"
		}
		out = append(out, stepRow{Title: st.Title, MS: ms, Class: stepClass(st), Depth: d})
	}
	return out, nav, nil
}

// shellQuote leaves a path alone only when every character is safe; anything else (spaces, newlines, quotes,
// globs, non-ASCII) puts it in single quotes, which a POSIX shell reads literally, newlines included. A path that
// starts with "-" gets "./" in front, since quoting does not stop a program reading it as an option.
func shellQuote(p string) string {
	if strings.HasPrefix(p, "-") {
		p = "./" + p
	}
	safe := p != ""
	for _, c := range p {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_@%+=:,./-", c)) {
			safe = false
			break
		}
	}
	if safe {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// screenshotPath is the screenshot the store knows for this test: the failure's own, or the last one recorded as an
// artifact. Only paths the store holds are ever served. The result has its links resolved and lies in the run's
// output dir; whoever serves it must open it with openArtifact, which checks that nothing changed since.
func (s *server) screenshotPath(r *http.Request, t store.Test) artifactPath {
	arts, err := s.st.Artifacts(r.Context(), t.Run)
	if err != nil {
		return artifactPath{}
	}
	want := parseFailure(t.Failure).Screenshot
	var found, last string
	for _, a := range arts {
		if a.Test == t.ID && a.Kind == "screenshot" {
			last = a.Path
			if a.Path == want {
				found = a.Path
			}
		}
	}
	if found == "" {
		found = last
	}
	if found == "" {
		return artifactPath{}
	}
	a, ok := s.inOutputDir(r.Context(), t.Run, found)
	if !ok {
		return artifactPath{}
	}
	return a
}

// artifactPath is a checked artifact: its resolved path and the shard output directory it lies in.
type artifactPath struct{ real, outDir string }

// beforeArtifactOpen lets a test change the file between the check and the open, the window a symlink swap needs.
var beforeArtifactOpen = func(string) {}

// openArtifact opens a checked artifact through a handle on its output directory, so the open cannot leave it: the
// directory itself must still be the one that was checked (not a link swapped in since), and no link on the way
// below it may point outside it. The opened file must be a regular file and the one the directory names. It returns
// nil when anything differs; the caller serves from the returned file and never from the path again.
func openArtifact(a artifactPath) *os.File {
	beforeArtifactOpen(a.real)
	rel, err := filepath.Rel(a.outDir, a.real)
	if err != nil || !filepath.IsLocal(rel) {
		return nil
	}
	root, err := os.OpenRoot(a.outDir)
	if err != nil {
		return nil
	}
	defer root.Close()
	heldDir, err1 := root.Stat(".")
	namedDir, err2 := os.Lstat(a.outDir)
	if err1 != nil || err2 != nil || !namedDir.IsDir() || !os.SameFile(heldDir, namedDir) {
		return nil
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil
	}
	held, err1 := f.Stat()
	named, err2 := root.Lstat(rel)
	if err1 != nil || err2 != nil || !held.Mode().IsRegular() || !named.Mode().IsRegular() || !os.SameFile(held, named) {
		f.Close()
		return nil
	}
	return f
}

// inOutputDir resolves path (symlinks and "..") and reports whether it lies inside the output directory of one of
// the run's shards (the `shard-<n>….out` folder next to the shard's log): a report is written by the code under test,
// so a path it names is never trusted to be somewhere safe to serve. It returns the resolved path and that folder.
func (s *server) inOutputDir(ctx context.Context, run, path string) (artifactPath, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return artifactPath{}, false
	}
	shards, err := s.st.Shards(ctx, run)
	if err != nil {
		return artifactPath{}, false
	}
	for _, sh := range shards {
		if sh.LogPath == "" {
			continue
		}
		dir, err := filepath.EvalSymlinks(filepath.Dir(sh.LogPath))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(dir, real)
		if err != nil {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 2 && parts[0] != ".." && strings.HasSuffix(parts[0], ".out") {
			return artifactPath{real: real, outDir: filepath.Join(dir, parts[0])}, true
		}
	}
	return artifactPath{}, false
}

func (s *server) screenshot(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.Run(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.st.RunTest(r.Context(), run.ID, r.PathValue("test"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := s.screenshotPath(r, t)
	ext := strings.ToLower(filepath.Ext(p.real))
	if p.real == "" || (ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp") {
		s.notFound(w, r)
		return
	}
	if !serveImage(w, r, p, run) {
		s.notFound(w, r)
	}
}

// serveImage serves the already-checked image a; it reports false, writing nothing, when the file
// cannot be opened safely.
func serveImage(w http.ResponseWriter, r *http.Request, a artifactPath, run store.Run) bool {
	f := openArtifact(a)
	if f == nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(strings.ToLower(filepath.Ext(a.real))))
	// A finished run never changes, so its screenshots are kept for a year; a live run's may still be replaced.
	if store.Terminal(run.State) {
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeContent(w, r, filepath.Base(a.real), info.ModTime(), f)
	return true
}

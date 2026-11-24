package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/triage"
)

// TriageTest is what triage made of one test of a run.
type TriageTest struct {
	Test   string            `json:"test"`
	Title  string            `json:"title"`
	File   string            `json:"file"`
	Line   int               `json:"line,omitempty"`
	Status string            `json:"status"`
	Class  string            `json:"class"`
	Lines  []string          `json:"lines"`
	Note   string            `json:"note,omitempty"`
	Patch  *triage.PatchInfo `json:"patch,omitempty"`
}

// TriageReport answers GET /api/runs/{id}/triage: every test of the run that has a class (a test that failed and
// then passed included), whether the run may be patched at all and how a patch may leave the machine.
type TriageReport struct {
	Run Run `json:"run"`
	// Propose is the suite's `triage.propose`: patch (default) or pr.
	Propose string `json:"propose"`
	// Fork says the run tests a fork's pull request: its patches are never applied or pushed for the user.
	Fork  bool         `json:"fork"`
	Tests []TriageTest `json:"tests"`
}

// FlakyList answers GET /api/flaky.
type FlakyList struct {
	Days  int               `json:"days"`
	Tests []store.FlakyTest `json:"tests"`
}

func (s *Server) triageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs/{id}/triage", s.triage)
	mux.HandleFunc("GET /api/flaky", s.flaky)
}

func (s *Server) triage(w http.ResponseWriter, r *http.Request) {
	run, ok := s.find(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	tests, err := s.d.Store.RunTests(ctx, run.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := TriageReport{Run: s.summarize(ctx, run), Propose: "patch", Tests: []TriageTest{}}
	if s.d.Suites != nil {
		if su, ok := s.d.Suites.Suite(run.Suite); ok {
			if c, err := triage.ParseConfig(su.Raw); err == nil {
				out.Propose = c.Propose
			}
		}
	}
	// The fork flag is on the run (T-10) and on the heads of the run, written by whoever admitted it.
	out.Fork = run.Fork
	var heads []struct {
		Fork bool `json:"fork"`
	}
	if len(run.Heads) > 0 {
		json.Unmarshal(run.Heads, &heads)
	}
	for _, h := range heads {
		out.Fork = out.Fork || h.Fork
	}
	for _, t := range tests {
		if t.Class == "" {
			continue
		}
		ev := triage.ReadEvidence(t.Evidence)
		lines := ev.Lines
		if lines == nil {
			lines = []string{}
		}
		out.Tests = append(out.Tests, TriageTest{Test: t.ID, Title: t.Title, File: t.File, Line: t.Line, Status: t.Status,
			Class: t.Class, Lines: lines, Note: ev.Note, Patch: ev.Patch})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) flaky(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			fail(w, http.StatusBadRequest, "days is a number from 1 to 3650, got %q", v)
			return
		}
		days = n
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	list, err := s.d.Store.Flaky(r.Context(), since)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if list == nil {
		list = []store.FlakyTest{}
	}
	writeJSON(w, http.StatusOK, FlakyList{Days: days, Tests: list})
}

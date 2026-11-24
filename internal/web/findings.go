package web

import (
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/store"
)

// maxFindingsShown bounds the page; older findings stay in the store and in `localci findings`.
const maxFindingsShown = 200

// findingsView is the Findings page: what the exploratory agent reported, one block per night, newest first.
type findingsView struct {
	Page
	Nights []night
	Total  int
}

type night struct {
	Run      string
	When     string // "Sat 21 Nov, 02:00"
	Latest   bool
	Findings []findingView
}

type findingView struct {
	store.Finding
	Open  bool   // the night's first finding opens by itself: steps and screenshot are the content
	Draft string // file name of the draft test; the path is on disk under the run
}

var severityRank = map[string]int{"high": 0, "medium": 1, "low": 2}

func (s *server) findings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := findingsView{Page: Page{Title: "Findings", Nav: "findings", Live: true}}
	v.Seq = s.seq(ctx)
	all, err := s.st.Findings(ctx, 0, 0, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(all) > maxFindingsShown {
		all = all[:maxFindingsShown]
	}
	v.Total = len(all)
	sort.SliceStable(all, func(a, b int) bool { return all[a].TS > all[b].TS })
	idx := map[string]int{}
	for _, f := range all { // newest first, so nights come out newest first
		i, ok := idx[f.Run]
		if !ok {
			i = len(v.Nights)
			idx[f.Run] = i
			v.Nights = append(v.Nights, night{Run: f.Run, When: time.UnixMilli(f.TS).Format("Mon 2 Jan, 15:04"), Latest: i == 0})
		}
		fv := findingView{Finding: f}
		if f.Test != "" {
			fv.Draft = filepath.Base(f.Test)
		}
		v.Nights[i].Findings = append(v.Nights[i].Findings, fv)
	}
	for i := range v.Nights {
		fs := v.Nights[i].Findings
		sort.SliceStable(fs, func(a, b int) bool { return severityRank[fs[a].Severity] < severityRank[fs[b].Severity] })
		fs[0].Open = i == 0
	}
	s.render(w, http.StatusOK, "findings", v)
}

// findingScreenshot serves a finding's screenshot after the same checks as a test's: a path inside the run's own
// output directory, a regular file, an image type.
func (s *server) findingScreenshot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	fs, err := s.st.Findings(r.Context(), id-1, 0, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var f *store.Finding
	for i := range fs {
		if fs[i].ID == id {
			f = &fs[i]
		}
	}
	if f == nil || f.Screenshot == "" {
		s.notFound(w, r)
		return
	}
	real, ok := s.inOutputDir(r.Context(), f.Run, f.Screenshot)
	ext := strings.ToLower(filepath.Ext(real.real))
	if !ok || (ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp") {
		s.notFound(w, r)
		return
	}
	run, err := s.st.Run(r.Context(), f.Run)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !serveImage(w, r, real, run) {
		s.notFound(w, r)
	}
}

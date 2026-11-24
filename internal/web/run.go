package web

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/nesolva/localci/internal/store"
)

type runView struct {
	Page
	R        store.Run
	Who      string
	Active   bool
	Started  int64
	Duration string
	Tab      string
	Attempt  attemptNav
	DAG      *dagGraph
	Lanes    []laneView
	Ticks    []tick
	Failures []failRow
	Tests    int
	Passed   int
	Shards   []logLink
}

type logLink struct {
	N     int
	State string
}

type tick struct {
	Left  float64
	Label string
}

type laneView struct {
	Label  string
	Serial bool
	Tests  []testBar
}

type testBar struct {
	ID    string
	Left  float64
	Width float64
	Class string // pass | fail | live | skip
	Tip   string
	Steps []stepSeg
}

type stepSeg struct {
	Left  float64 // % of the test bar
	Width float64
	Class string
	Tip   string
	Flows string // keys of the flows called by this step or inside it, for the DAG's highlight
}

type failRow struct {
	ID      string
	Title   string
	Step    string
	Message string
	Shard   int
	Class   string
}

func (s *server) run(w http.ResponseWriter, r *http.Request) { s.runPage(w, r, "timeline") }

func (s *server) runDAG(w http.ResponseWriter, r *http.Request) { s.runPage(w, r, "dag") }

func (s *server) runPage(w http.ResponseWriter, r *http.Request, tab string) {
	ctx := r.Context()
	id := r.PathValue("id")
	run, err := s.st.Run(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v := runView{Page: Page{Title: run.Suite + " " + shortID(run.ID), Run: run.ID, Live: !store.Terminal(run.State)}, R: run,
		Who: who(run.Trigger, run.RequestedBy), Active: !store.Terminal(run.State), Tab: tab}
	v.Seq = s.seq(ctx)
	v.Started = run.Started
	if v.Started == 0 {
		v.Started = run.Created
	}
	if run.Finished > 0 && run.Started > 0 {
		v.Duration = fmtMS(run.Finished - run.Started)
	} else if v.Active {
		v.Duration = fmtMS(s.now().UnixMilli() - v.Started)
	}

	evs, err := s.st.RunEvents(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := parseRunEvents(evs)
	sel := attemptParam(r)
	var lanes []lane
	if data.hasSteps() {
		lanes = data.timeLanes(sel, v.Active, run.Started)
	} else { // steps stored without events (older runs)
		sl, err := s.st.Timeline(ctx, run.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		lanes = lanesFromStore(sl, v.Active)
	}
	v.Attempt = newAttemptNav(r, data.maxAttempts(), sel)
	tests, err := s.st.RunTests(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	serial, err := s.st.Serial(ctx, run.Suite)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	shards, err := s.st.Shards(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, sh := range shards {
		if sh.LogPath != "" {
			v.Shards = append(v.Shards, logLink{N: sh.N, State: sh.State})
		}
	}
	byID := make(map[string]store.Test, len(tests))
	for _, t := range tests {
		byID[t.ID] = t
		v.Tests++
		if lower(t.Status) == "passed" {
			v.Passed++
		}
	}
	serialIDs := map[string]bool{}
	for _, e := range serial {
		serialIDs[e.Test] = true
	}
	flowsOf := map[string]string{}
	if tab == "dag" {
		v.DAG = buildDAG(data, sel, byID, v.Active)
		flowsOf = v.DAG.stepFlows
	}
	v.Lanes, v.Ticks = s.layout(lanes, byID, serialIDs, flowsOf, v.Active, run)

	fails, err := s.st.Failures(ctx, run.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, t := range fails {
		f := parseFailure(t.Failure)
		v.Failures = append(v.Failures, failRow{ID: t.ID, Title: t.Title, Step: f.Step, Message: firstLine(f.Message), Shard: t.Shard, Class: t.Class})
	}
	page := "run"
	if tab == "dag" {
		page = "dag"
	}
	s.render(w, http.StatusOK, page, v)
}

// layout places every test on its shard's lane (serial tests on their own lane) as a bar split into its top-level
// steps. Positions are percentages of the run's span, so the page needs no script to draw.
func (s *server) layout(lanes []lane, byID map[string]store.Test, serial map[string]bool, flows map[string]string, active bool, run store.Run) ([]laneView, []tick) {
	type span struct{ start, end int64 }
	var total int64
	type group struct {
		id    string
		steps []tstep
		span  span
	}
	build := func(steps []tstep) []group {
		idx := map[string]int{}
		var gs []group
		for _, st := range steps {
			if st.Parent != "" {
				continue // nested steps are inside their parent's bar
			}
			i, ok := idx[st.Test]
			if !ok {
				i = len(gs)
				idx[st.Test] = i
				gs = append(gs, group{id: st.Test, span: span{start: st.Offset}})
			}
			end := st.Offset + st.MS
			gs[i].steps = append(gs[i].steps, st)
			if end > gs[i].span.end {
				gs[i].span.end = end
			}
		}
		return gs
	}
	// A step still running reaches "now"; a finished run is as long as its last step.
	var nowOff int64
	if active {
		base := run.Started
		if base == 0 {
			base = run.Created
		}
		nowOff = s.now().UnixMilli() - base
	}
	type laneGroups struct {
		label  string
		serial bool
		gs     []group
	}
	var all []laneGroups
	var serialGroups []group
	for _, l := range lanes {
		var keep []tstep
		var ser []tstep
		for _, st := range l.Steps {
			if serial[st.Test] {
				ser = append(ser, st)
			} else {
				keep = append(keep, st)
			}
		}
		if g := build(keep); len(g) > 0 {
			label := fmt.Sprintf("Shard %d", l.Shard)
			if l.Shard == 0 {
				label = "Run"
			}
			all = append(all, laneGroups{label: label, gs: g})
		}
		serialGroups = append(serialGroups, build(ser)...)
	}
	if len(serialGroups) > 0 {
		sort.SliceStable(serialGroups, func(i, j int) bool { return serialGroups[i].span.start < serialGroups[j].span.start })
		all = append(all, laneGroups{label: "Serial", serial: true, gs: serialGroups})
	}
	for _, l := range all {
		for gi := range l.gs {
			g := &l.gs[gi]
			for i := range g.steps {
				switch {
				case g.steps[i].Open && g.steps[i].MS == 0:
					// Never ended and nothing runs it: it reaches the end of what its test did.
					g.steps[i].MS = max(g.span.end-g.steps[i].Offset, 1)
				case g.steps[i].OK == nil && active && nowOff > g.steps[i].Offset:
					g.steps[i].MS = nowOff - g.steps[i].Offset
					g.span.end = max(g.span.end, g.steps[i].Offset+g.steps[i].MS)
				}
			}
			total = max(total, g.span.end)
		}
	}
	if active {
		total = max(total, nowOff)
	}
	if total <= 0 {
		total = 1
	}
	pct := func(ms int64) float64 { return float64(ms) * 100 / float64(total) }

	out := make([]laneView, 0, len(all))
	for _, l := range all {
		lv := laneView{Label: l.label, Serial: l.serial, Tests: make([]testBar, 0, len(l.gs))}
		for _, g := range l.gs {
			t := byID[g.id]
			dur := g.span.end - g.span.start
			if dur <= 0 {
				dur = 1
			}
			tb := testBar{ID: g.id, Left: pct(g.span.start), Width: pct(dur), Class: testClass(t, g.steps)}
			title := t.Title
			if title == "" {
				title = g.id
			}
			tb.Tip = fmt.Sprintf("%s — %s", title, fmtMS(g.span.end-g.span.start))
			for _, st := range g.steps {
				ms := max(st.MS, 1)
				sg := stepSeg{Left: float64(st.Offset-g.span.start) * 100 / float64(dur), Width: float64(ms) * 100 / float64(dur), Class: stepClass(st), Flows: flows[st.Test+"|"+st.ID]}
				sg.Tip = fmt.Sprintf("%s — %s", st.Title, fmtMS(st.MS))
				if st.Open {
					sg.Tip = st.Title + " — did not finish"
				}
				tb.Steps = append(tb.Steps, sg)
			}
			lv.Tests = append(lv.Tests, tb)
		}
		out = append(out, lv)
	}

	var ticks []tick
	step := niceStep(total)
	for ms := int64(0); ms <= total; ms += step {
		ticks = append(ticks, tick{Left: pct(ms), Label: fmtMS(ms)})
	}
	return out, ticks
}

func testClass(t store.Test, steps []tstep) string {
	switch {
	case isFailed(t.Status):
		return "fail"
	case lower(t.Status) == "skipped":
		return "skip"
	case isDone(t.Status):
		return "pass"
	}
	for _, st := range steps {
		if st.OK != nil && !*st.OK {
			return "fail"
		}
	}
	return "live"
}

func stepClass(st tstep) string {
	c := "k-" + st.Kind
	switch {
	case st.Open:
		c += " is-open"
	case st.OK == nil:
		c += " is-live"
	case !*st.OK:
		c += " is-fail"
	}
	return c
}

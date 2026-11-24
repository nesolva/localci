package web

import (
	"net/http"
	"sort"
	"strings"

	"github.com/nesolva/localci/internal/store"
)

type overviewView struct {
	Page
	Running []runCard
	Queue   []queueRow
	Recent  []recentRow
	Idle    bool
}

type runCard struct {
	ID        string
	Suite     string
	Check     bool
	Trigger   string
	By        string
	Started   int64
	Elapsed   string
	Left      string // "" when unknown
	Done      int
	Total     int
	Failed    int
	Preparing bool
	Shards    []shardBar
}

type shardBar struct {
	N       int
	Done    int
	Total   int
	Failed  int
	Pct     float64
	FailPct float64
}

type queueRow struct {
	Pos     int
	ID      string
	Suite   string
	Trigger string
	By      string
	Since   int64
}

type recentRow struct {
	ID       string
	Suite    string
	Trigger  string
	By       string
	State    string
	Reason   string
	Failures int
	Duration string
	Finished int64
}

func (s *server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := overviewView{Page: Page{Title: "Overview", Nav: "overview", Live: true}}
	v.Seq = s.seq(ctx)
	runs, err := s.st.Runs(ctx, 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, run := range runs {
		switch run.State {
		case store.StateRunning, store.StatePreparing:
			card, err := s.card(r, run)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			v.Running = append(v.Running, card)
		case store.StateQueued:
			v.Queue = append(v.Queue, queueRow{ID: run.ID, Suite: run.Suite, Trigger: run.Trigger, By: run.RequestedBy, Since: run.Created})
		default:
			if len(v.Recent) >= 15 {
				continue
			}
			row := recentRow{ID: run.ID, Suite: run.Suite, Trigger: run.Trigger, By: run.RequestedBy, State: run.State,
				Reason: run.Reason, Finished: run.Finished}
			if run.Finished > 0 && run.Started > 0 {
				row.Duration = fmtMS(run.Finished - run.Started)
			}
			if run.State == store.StateFailed {
				fs, err := s.st.Failures(ctx, run.ID)
				if err != nil {
					s.fail(w, r, err)
					return
				}
				row.Failures = len(fs)
			}
			v.Recent = append(v.Recent, row)
		}
	}
	// Longest-running first: that is the one a person is waiting on.
	sort.SliceStable(v.Running, func(i, j int) bool { return v.Running[i].Started < v.Running[j].Started })
	// The queue is served oldest first.
	sort.SliceStable(v.Queue, func(i, j int) bool { return v.Queue[i].Since < v.Queue[j].Since })
	for i := range v.Queue {
		v.Queue[i].Pos = i + 1
	}
	v.Idle = len(v.Running) == 0
	s.render(w, http.StatusOK, "overview", v)
}

// card is one running run: who sent it, how long it has been going, how long is left, and each shard's progress.
func (s *server) card(r *http.Request, run store.Run) (runCard, error) {
	ctx := r.Context()
	c := runCard{ID: run.ID, Suite: run.Suite, Check: run.Kind == store.KindCheck, Trigger: run.Trigger, By: run.RequestedBy,
		Started: run.Started, Preparing: run.State == store.StatePreparing}
	if c.Started == 0 {
		c.Started = run.Created
	}
	c.Elapsed = fmtMS(s.now().UnixMilli() - c.Started)
	shards, err := s.st.Shards(ctx, run.ID)
	if err != nil {
		return c, err
	}
	tests, err := s.st.RunTests(ctx, run.ID)
	if err != nil {
		return c, err
	}
	byShard := map[int]*shardBar{}
	for _, sh := range shards {
		byShard[sh.N] = &shardBar{N: sh.N}
	}
	for _, t := range tests {
		b := byShard[t.Shard]
		if b == nil {
			b = &shardBar{N: t.Shard}
			byShard[t.Shard] = b
		}
		b.Total++
		if isDone(t.Status) {
			b.Done++
		}
		if isFailed(t.Status) {
			b.Failed++
		}
	}
	for _, b := range byShard {
		if b.Total > 0 {
			b.Pct = float64(b.Done) * 100 / float64(b.Total)
			b.FailPct = float64(b.Failed) * 100 / float64(b.Total)
		}
		c.Done += b.Done
		c.Total += b.Total
		c.Failed += b.Failed
		c.Shards = append(c.Shards, *b)
	}
	sort.Slice(c.Shards, func(i, j int) bool { return c.Shards[i].N < c.Shards[j].N })
	c.Left = s.timeLeft(r, tests)
	return c, nil
}

// timeLeft estimates what remains from measured medians (the mean of the run's own finished tests for a test with no
// history); the slowest shard decides. Empty when there is nothing to base it on.
func (s *server) timeLeft(r *http.Request, tests []store.Test) string {
	var ids []string
	var doneMS, doneN int64
	for _, t := range tests {
		ids = append(ids, t.ID)
		if isDone(t.Status) && t.MS > 0 {
			doneMS += t.MS
			doneN++
		}
	}
	med, _ := s.st.MedianDurations(r.Context(), ids)
	var fallback int64
	if doneN > 0 {
		fallback = doneMS / doneN
	}
	remaining := map[int]int64{}
	var known bool
	for _, t := range tests {
		if isDone(t.Status) {
			continue
		}
		d := med[t.ID]
		if d == 0 {
			d = fallback
		}
		if d > 0 {
			known = true
		}
		remaining[t.Shard] += d
	}
	if !known {
		return ""
	}
	var worst int64
	for _, ms := range remaining {
		worst = max(worst, ms)
	}
	if worst < 1000 {
		return "< 1 s"
	}
	return "~" + fmtMS(worst)
}

// who renders "trigger · by" without dangling separators.
func who(trigger, by string) string {
	parts := make([]string, 0, 2)
	for _, p := range []string{trigger, by} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " · ")
}

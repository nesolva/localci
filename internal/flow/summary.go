package flow

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"

	"github.com/nesolva/localci/internal/report"
	"github.com/nesolva/localci/internal/store"
)

// Summary is one flow as `localci flows` shows it, learned from the flow events of past runs.
type Summary struct {
	Name string `json:"name"`
	// Inputs and Outputs are the names the flow takes and returns, each with the value of its latest call, redacted
	// like the events they come from.
	Inputs  map[string]any `json:"inputs"`
	Outputs map[string]any `json:"outputs"`
	Tests   int            `json:"tests"` // distinct tests that called it
	Calls   int            `json:"calls"`
	// RunMS is the median time of calls that ran the flow, RestoreMS of those that restored a checkpoint.
	RunMS     int64 `json:"run_ms"`
	RestoreMS int64 `json:"restore_ms"`
	// Checkpointed counts the calls that could use a checkpoint (it was saved or restored); HitRate is the share of
	// them that restored one.
	Checkpointed int     `json:"checkpointed"`
	HitRate      float64 `json:"hit_rate"`
}

// Kinds are the event kinds Summaries reads.
var Kinds = []string{"flow.begin", "flow.end"}

type begin struct {
	Flow   string         `json:"flow"`
	Inputs map[string]any `json:"inputs"`
}
type end struct {
	Outputs    map[string]any `json:"outputs"`
	Checkpoint string         `json:"checkpoint"`
	MS         float64        `json:"ms"`
}

// Summaries folds flow.begin and flow.end events (oldest first) into one summary per flow, by name.
func Summaries(events []store.Event) []Summary {
	type acc struct {
		s        Summary
		tests    map[string]bool
		run, rst []int64
		hit, can int
	}
	byName := map[string]*acc{}
	open := map[string]*acc{} // run/step -> the flow it began
	for _, ev := range events {
		switch ev.Kind {
		case "flow.begin":
			var b begin
			if json.Unmarshal(ev.Data, &b) != nil || b.Flow == "" {
				continue
			}
			a := byName[b.Flow]
			if a == nil {
				a = &acc{s: Summary{Name: b.Flow}, tests: map[string]bool{}}
				byName[b.Flow] = a
			}
			a.s.Calls++
			a.tests[ev.Test] = true
			a.s.Inputs = report.Redact(b.Inputs)
			open[ev.Run+"/"+ev.Step] = a
		case "flow.end":
			a := open[ev.Run+"/"+ev.Step]
			if a == nil {
				continue
			}
			delete(open, ev.Run+"/"+ev.Step)
			var e end
			if json.Unmarshal(ev.Data, &e) != nil {
				continue
			}
			if e.Outputs != nil {
				a.s.Outputs = report.Redact(e.Outputs)
			}
			switch e.Checkpoint {
			case "restored":
				a.rst = append(a.rst, int64(e.MS))
				a.hit++
				a.can++
			case "saved":
				a.run = append(a.run, int64(e.MS))
				a.can++
			default:
				a.run = append(a.run, int64(e.MS))
			}
		}
	}
	out := make([]Summary, 0, len(byName))
	for _, a := range byName {
		a.s.Tests = len(a.tests)
		a.s.RunMS, a.s.RestoreMS = median(a.run), median(a.rst)
		a.s.Checkpointed = a.can
		if a.can > 0 {
			a.s.HitRate = float64(a.hit) / float64(a.can)
		}
		if a.s.Inputs == nil {
			a.s.Inputs = map[string]any{}
		}
		if a.s.Outputs == nil {
			a.s.Outputs = map[string]any{}
		}
		out = append(out, a.s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func median(v []int64) int64 {
	if len(v) == 0 {
		return 0
	}
	v = slices.Clone(v)
	slices.Sort(v)
	return v[len(v)/2]
}

// maxEvents is how many of the newest flow events a listing reads.
const maxEvents = 50_000

// APIHandler serves GET /api/flows: the flows the recent runs called.
func APIHandler(st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		evs, err := st.KindEvents(context.Background(), maxEvents, Kinds...)
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"flows": Summaries(evs)})
	})
}

package web

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/nesolva/localci/internal/store"
)

// The DAG of a run: flow instances as nodes keyed by checkpoint key, edges from the call that ran before to the one
// that follows. A flow's key already contains its parent's, so "before" is exactly what the key says: the enclosing
// flow for the first call inside it, the previous sibling for the next ones. Tests are leaves, hung on the last flow
// their body called. The layout is layered, left to right, and computed here, so the page needs no graph library.

const (
	nodeW     = 200
	nodeGap   = 48
	rowGap    = 12
	nodeHead  = 88 // name, inputs, counts, bar: fixed lines, so a card has a known height
	dotPitch  = 24 // a test dot is drawn 10px inside a 24px hit target
	dotsWidth = nodeW - 24
	maxDotRow = 4 // more tests than this many rows of dots become "+N"
	padX      = 4
	padY      = 4
)

type dagGraph struct {
	Nodes []*dagNode
	Edges []dagEdge
	W, H  int

	Calls, Restored int // flow calls of the shown attempts, and how many came from a checkpoint
	Tests           int // tests hung on a flow
	Loose           int // tests that called no flow

	stepFlows map[string]string // test|step -> keys of the flows called by that step or inside it
}

type dagNode struct {
	Key, Name, Inputs string
	Layer             int
	X, Y, H           int
	State             string // ran | restored | failed | live
	Calls, Restored   int
	MS                int64   // all the time this flow took in the run
	Bar               float64 // % of the slowest flow
	Meta, Time        string
	Tests             []dagLeaf
	More              int

	preds []string
	order float64
}

type dagLeaf struct {
	ID, Title, Class string
}

type edgePair struct{ from, to string }

type dagEdge struct {
	From, To string
	Path     string
}

func buildDAG(d *runData, sel int, byID map[string]store.Test, active bool) *dagGraph {
	g := &dagGraph{stepFlows: map[string]string{}}
	nodes := map[string]*dagNode{}
	var list []*dagNode
	seen := map[edgePair]bool{}
	failedNode := map[string]bool{}
	liveNode := map[string]bool{}
	var edges []edgePair

	for _, test := range d.order {
		a := d.pick(test, sel)
		if a == nil {
			continue
		}
		latest := a == d.last(test)
		lastIn := map[string]*flowInst{} // frame (enclosing flow step, "" at the top) -> last flow called in it
		for _, f := range a.Flows {
			enclosing := ""
			for p, n := f.Parent, 0; p != "" && n < 64; n++ {
				if _, ok := a.flows[p]; ok {
					enclosing = p
					break
				}
				st := a.steps[p]
				if st == nil {
					break
				}
				p = st.Parent
			}
			var pred *flowInst
			if p := lastIn[enclosing]; p != nil {
				pred = p
			} else if enclosing != "" {
				pred = a.flows[enclosing]
			}
			lastIn[enclosing] = f

			n := nodes[f.Key]
			if n == nil {
				n = &dagNode{Key: f.Key, Name: f.Flow, Inputs: inputsLine(f.Inputs)}
				nodes[f.Key] = n
				list = append(list, n)
			}
			n.Calls++
			g.Calls++
			if f.Checkpoint == "restored" {
				n.Restored++
				g.Restored++
			}
			n.MS += f.MS
			if st := a.steps[f.Step]; st != nil && st.OK != nil && !*st.OK {
				failedNode[f.Key] = true
			}
			if !f.Ended && active && latest {
				liveNode[f.Key] = true
			}
			if pred != nil && pred.Key != f.Key {
				ek := edgePair{pred.Key, f.Key}
				if !seen[ek] {
					seen[ek] = true
					edges = append(edges, ek)
				}
			}
			// The flow's step and every step above it are marked, so the timeline can light them up.
			for p, i := f.Step, 0; p != "" && i < 64; i++ {
				k := test + "|" + p
				if !strings.Contains(" "+g.stepFlows[k]+" ", " "+f.Key+" ") {
					g.stepFlows[k] = strings.TrimSpace(g.stepFlows[k] + " " + f.Key)
				}
				st := a.steps[p]
				if st == nil {
					break
				}
				p = st.Parent
			}
		}
		if top := lastIn[""]; top != nil {
			t := byID[test]
			title := t.Title
			if title == "" {
				title = test
			}
			n := nodes[top.Key]
			if len(n.Tests) >= maxDotRow*(dotsWidth/dotPitch) {
				n.More++
			} else {
				n.Tests = append(n.Tests, dagLeaf{ID: test, Title: title, Class: leafClass(t)})
			}
			g.Tests++
		} else if len(a.Flows) == 0 {
			g.Loose++
		}
	}

	var maxMS int64
	for _, n := range list {
		maxMS = max(maxMS, n.MS)
	}
	for _, n := range list {
		switch {
		case failedNode[n.Key]:
			n.State = "failed"
		case liveNode[n.Key]:
			n.State = "live"
		case n.Calls > 0 && n.Restored == n.Calls:
			n.State = "restored"
		default:
			n.State = "ran"
		}
		if maxMS > 0 {
			n.Bar = max(float64(n.MS)*100/float64(maxMS), 1.5)
		}
		n.Meta, n.Time = nodeMeta(n), fmtMS(n.MS)
	}
	for _, e := range edges {
		nodes[e.to].preds = append(nodes[e.to].preds, e.from)
	}
	g.Nodes = list
	g.layout(nodes, edges)
	return g
}

func leafClass(t store.Test) string {
	switch {
	case isFailed(t.Status):
		return "is-fail"
	case lower(t.Status) == "skipped":
		return "is-skip"
	case isDone(t.Status):
		return "pass"
	}
	return "is-live"
}

// nodeMeta counts the calls: how many ran and how many came back from a checkpoint. The time is shown apart.
func nodeMeta(n *dagNode) string {
	ran := n.Calls - n.Restored
	switch {
	case n.State == "live" && n.MS == 0:
		return "running"
	case n.Restored == n.Calls:
		return fmt.Sprintf("restored ×%d", n.Restored)
	case n.Restored == 0:
		return fmt.Sprintf("ran ×%d", ran)
	}
	return fmt.Sprintf("ran ×%d · restored ×%d", ran, n.Restored)
}

// inputsLine shows a flow's (already redacted) inputs as k=v pairs in key order, cut at 60 characters.
func inputsLine(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%v", k, in[k])
	}
	s := b.String()
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// layout assigns every node a layer (the longest chain of flows before it), orders each layer by where its
// predecessors sit, and gives each node a position and each edge a path.
func (g *dagGraph) layout(nodes map[string]*dagNode, edges []edgePair) {
	if len(g.Nodes) == 0 {
		return
	}
	// Longest path, relaxed at most once per node: a cycle (two runs of the same flows in opposite order) stops
	// growing instead of looping.
	for i := 0; i < len(g.Nodes); i++ {
		changed := false
		for _, e := range edges {
			if nodes[e.to].Layer < nodes[e.from].Layer+1 && nodes[e.from].Layer < len(g.Nodes) {
				nodes[e.to].Layer = nodes[e.from].Layer + 1
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	layers := map[int][]*dagNode{}
	deepest := 0
	for _, n := range g.Nodes {
		l := min(n.Layer, len(g.Nodes)-1)
		n.Layer = l
		layers[l] = append(layers[l], n)
		deepest = max(deepest, l)
	}
	idx := map[string]int{}
	for l := 0; l <= deepest; l++ {
		col := layers[l]
		for _, n := range col {
			if len(n.preds) > 0 {
				var sum float64
				for _, p := range n.preds {
					sum += float64(idx[p])
				}
				n.order = sum / float64(len(n.preds))
			}
		}
		sort.SliceStable(col, func(i, j int) bool { return col[i].order < col[j].order })
		for i, n := range col {
			idx[n.Key] = i
		}
		y := padY
		for _, n := range col {
			rows := 0
			if len(n.Tests) > 0 {
				rows = (len(n.Tests) + dotsWidth/dotPitch - 1) / (dotsWidth / dotPitch)
			}
			n.H = nodeHead + rows*dotPitch
			if n.More > 0 {
				n.H += 18
			}
			n.X, n.Y = padX+l*(nodeW+nodeGap), y
			y += n.H + rowGap
			g.H = max(g.H, y)
		}
	}
	g.W = padX*2 + (deepest+1)*nodeW + deepest*nodeGap
	g.H += padY
	for _, e := range edges {
		a, b := nodes[e.from], nodes[e.to]
		if b.Layer <= a.Layer {
			continue
		}
		x1, y1 := a.X+nodeW, a.Y+21
		x2, y2 := b.X, b.Y+21
		mx := (x1 + x2) / 2
		g.Edges = append(g.Edges, dagEdge{From: a.Key, To: b.Key,
			Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, mx, y1, mx, y2, x2, y2)})
	}
}

// attemptNav is the selector shown when any test ran more than once.
type attemptNav struct {
	Q     string // the query that keeps the choice when moving between tabs
	Show  bool
	Items []attemptItem
}

type attemptItem struct {
	Label, Href string
	Current     bool
}

// attemptParam reads ?attempt=N (1-based) as a 0-based attempt; absent or invalid is -1, the last.
func attemptParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("attempt"))
	if err != nil || n < 1 || n > 1000 {
		return -1
	}
	return n - 1
}

func newAttemptNav(r *http.Request, most, sel int) attemptNav {
	if most < 2 {
		return attemptNav{}
	}
	q := ""
	if sel >= 0 {
		q = "?attempt=" + strconv.Itoa(sel+1)
	}
	href := func(n int) string {
		q := url.Values{}
		if n > 0 {
			q.Set("attempt", strconv.Itoa(n))
		}
		if len(q) == 0 {
			return r.URL.Path
		}
		return r.URL.Path + "?" + q.Encode()
	}
	nav := attemptNav{Q: q, Show: true, Items: []attemptItem{{Label: "Last", Href: href(0), Current: sel < 0}}}
	for n := 1; n <= most; n++ {
		nav.Items = append(nav.Items, attemptItem{Label: strconv.Itoa(n), Href: href(n), Current: sel == n-1})
	}
	return nav
}

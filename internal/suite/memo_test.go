package suite

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/memo"
)

// Two sibling subtrees share their first flow; the rest run without flows or fail on purpose.
var memoIDs = []string{
	"memo.login.order-a.t1", "memo.login.order-a.t2", "memo.login.order-a.t3",
	"memo.login.order-b.t1", "memo.login.order-b.t2", "memo.login.order-b.t3",
	"memo.login.order-c.t1", "memo.login.order-c.t2",
	"memo.signup.t1", "memo.signup.t2",
	"pass1", "pass2", "broken1", "ghost1",
}

func (g *rig) flowCalls() (saved, restored []string) {
	for _, l := range readLines(filepath.Join(g.shared, "flows.log")) {
		f := strings.Fields(l)
		if f[0] == "saved" {
			saved = append(saved, f[1])
		} else {
			restored = append(restored, f[1])
		}
	}
	return
}

// A run starts from no checkpoints, like the first run on a new commit.
func (g *rig) freshCheckpoints() {
	g.t.Helper()
	ents, _ := filepath.Glob(filepath.Join(g.shared, "ck-*"))
	for _, e := range ents {
		os.Remove(e)
	}
	os.Remove(filepath.Join(g.shared, "flows.log"))
}

func (g *rig) outcomes(run string) map[string]string {
	g.t.Helper()
	ts, err := g.st.RunTests(context.Background(), run)
	if err != nil {
		g.t.Fatal(err)
	}
	m := map[string]string{}
	for _, tc := range ts {
		m[tc.ID] = tc.Status
	}
	return m
}

// memoSuite is the fake suite on three shards.
func memoSuite(extra string) string {
	return strings.Replace(fakeSuite(extra), "shards: 2", "shards: 3", 1)
}

const memoOff = "    memo: false\n"

// A memoized run reports exactly what an unmemoized one does, run after run, with a test that fails and one that
// never reports; and it builds every distinct prefix once, however many shards want it.
func TestAMemoizedRunReportsWhatAPlainRunDoesAndBuildsEachPrefixOnce(t *testing.T) {
	plain := newRig(t, memoSuite(memoOff), memoIDs, 4)
	memoized := newRig(t, memoSuite(""), memoIDs, 4)

	want := map[string]string{}
	for i := range 5 {
		plain.freshCheckpoints()
		p := plain.run(Request{Suite: "e2e", Heads: mainHead()})
		got := plain.outcomes(p.ID)
		if i == 0 {
			want = got
			if want["broken1"] == "passed" || want["memo.login.order-a.t1"] != "passed" {
				t.Fatalf("baseline outcomes = %v", want)
			}
		}

		memoized.freshCheckpoints()
		m := memoized.run(Request{Suite: "e2e", Heads: mainHead()})
		if m.State != p.State {
			t.Fatalf("run %d: memoized %s, plain %s", i, m.State, p.State)
		}
		if mine := memoized.outcomes(m.ID); !equalMaps(mine, want) {
			t.Fatalf("run %d: memoized outcomes differ from the plain run\n memoized %v\n plain    %v", i, mine, want)
		}
		saved, _ := memoized.flowCalls()
		if i == 0 {
			continue // the first run learns the prefixes: it runs as a plain one
		}
		slices.Sort(saved)
		wantNodes := []string{"login", "login.order-a", "login.order-b", "login.order-c", "signup"}
		if !slices.Equal(saved, wantNodes) {
			t.Fatalf("run %d: flows built %v, want each distinct prefix once %v", i, saved, wantNodes)
		}
	}
}

// What a fork's report says never changes what later runs plan with, though a fork uses what trusted runs learned.
func TestAForkRunDoesNotChangeThePrefixTree(t *testing.T) {
	g := newRig(t, memoSuite(""), memoIDs[:4], 4)
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))
	dir := filepath.Join(g.data, "state", "e2e")
	before, err := memo.Load(dir)
	if err != nil || len(before) != 4 {
		t.Fatalf("learned prefixes = %v, %v, want 4 tests", before, err)
	}
	g.commit(map[string]string{"tests.txt": strings.Join(append(slices.Clone(memoIDs[:4]), "memo.evil.x.t1"), "\n") + "\n"})
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead(), Fork: true}))
	if after, _ := memo.Load(dir); !equalMaps2(after, before) {
		t.Fatalf("a fork run changed the prefixes: %v -> %v", before, after)
	}
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))
	if after, _ := memo.Load(dir); len(after["memo.evil.x.t1"]) != 2 {
		t.Fatalf("a trusted run did not learn the new test: %v", after)
	}
}

// memo: false turns learning off too.
func TestMemoFalseLearnsNothing(t *testing.T) {
	g := newRig(t, memoSuite(memoOff), memoIDs[:4], 4)
	g.mustPass(g.run(Request{Suite: "e2e", Heads: mainHead()}))
	if got, _ := memo.Load(filepath.Join(g.data, "state", "e2e")); len(got) != 0 {
		t.Fatalf("memo: false learned %v", got)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func equalMaps2(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(b[k], v) {
			return false
		}
	}
	return true
}

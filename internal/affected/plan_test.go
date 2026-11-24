package affected

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@localhost"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"+t.Name()+"\n"), 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// The diff is the pull request's own: what its branch changed since it left the base, not what the base did
// meanwhile; and the map is as old as the base commits since it was measured.
func TestPlanDiffsAgainstTheMergeBaseAndAgesTheMapOnTheBase(t *testing.T) {
	src := t.TempDir()
	gitIn(t, src, "init", "-q", "-b", "main")
	c1 := commitFile(t, src, "public/cart.js")
	gitIn(t, src, "checkout", "-q", "-b", "feat")
	head := commitFile(t, src, "public/pay.js")
	gitIn(t, src, "checkout", "-q", "main")
	commitFile(t, src, "public/other.js") // the base moves on

	ctx := context.Background()
	m, err := exec.EnsureMirror(ctx, t.TempDir(), shop, src)
	if err != nil {
		t.Fatal(err)
	}
	p, st := newPlanner(t)
	st.PutCoverage(ctx, "e2e", []store.CoverageEntry{
		{Test: "t-cart", Files: []string{shop + ":public/cart.js"}, Commits: map[string]string{shop: c1}},
		{Test: "t-pay", Files: []string{shop + ":public/pay.js"}, Commits: map[string]string{shop: c1}},
	}, nil)

	in := suite.PlanInput{Run: "1", Suite: testSuite(), Repos: map[string]suite.RepoState{shop: {Mirror: m, Head: head}},
		Tests: []suite.TestRef{{ID: "t-cart", File: "tests/cart.spec.ts"}, {ID: "t-pay", File: "tests/pay.spec.ts"}}}
	plan, err := p.Plan(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var sel Selection
	json.Unmarshal(plan.Detail, &sel)
	if plan.All || len(plan.IDs) != 1 || plan.IDs[0] != "t-pay" {
		t.Fatalf("plan = %+v (%s), want only t-pay: the base's own change is not this branch's", plan, sel.Reason)
	}
	if len(sel.Changed) != 1 || sel.Changed[0] != shop+":public/pay.js" {
		t.Fatalf("changed = %v", sel.Changed)
	}

	// 201 commits later the map is too old to trust.
	for i := 0; i <= MaxAge; i++ {
		gitIn(t, src, "commit", "-q", "--allow-empty", "-m", "later")
	}
	plan, err = p.Plan(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(plan.Detail, &sel)
	if !plan.All || !strings.Contains(sel.Reason, "200 commits") {
		t.Fatalf("plan = %+v (%s), want every test: the map is %d commits behind", plan, sel.Reason, MaxAge+2)
	}
}

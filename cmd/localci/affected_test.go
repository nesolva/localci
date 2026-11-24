package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/affected"
	"github.com/nesolva/localci/internal/event"
	"github.com/nesolva/localci/internal/sched"
	"github.com/nesolva/localci/internal/server"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

// `run --affected --explain` on a working tree with an uncommitted change: it runs no test and says which tests the
// change reaches and why, from the coverage map earlier runs left.
func TestRunAffectedExplainSaysWhyEachTest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	repo := t.TempDir()
	sh := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for _, f := range []string{"public/cart.js", "public/pay.js", "tests/cart.spec.ts", "tests/pay.spec.ts"} {
		os.MkdirAll(filepath.Dir(filepath.Join(repo, f)), 0o755)
		os.WriteFile(filepath.Join(repo, f), []byte("old\n"), 0o644)
	}
	sh("init", "-q", "-b", "main")
	sh("add", ".")
	sh("commit", "-q", "-m", "first")
	sh("remote", "add", "origin", "https://github.com/acme/web.git")
	base, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "public/pay.js"), []byte("new\n"), 0o644) // uncommitted, tracked

	suites, err := suite.ParseYAML([]byte(`suites:
  e2e:
    repos: [acme/web]
    test:
      run: "false"
      list: |
        printf '{"test":"t-cart","file":"tests/cart.spec.ts","title":"cart","line":1}\n{"test":"t-pay","file":"tests/pay.spec.ts","title":"pay","line":1}\n'
`))
	if err != nil {
		t.Fatal(err)
	}
	url, st := realServer(t, func(d *server.Deps) {
		r, err := suite.New(suite.Deps{Store: d.Store, Log: event.NewLog(d.Store, d.Bus), Sched: sched.New(2, 0), DataDir: d.DataDir,
			Suites: suites, RepoURL: func(string) string { return repo }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(r.Close)
		if err := (&affected.Planner{Store: d.Store}).Register(r); err != nil {
			t.Fatal(err)
		}
		d.Suites = r
	})
	commits := map[string]string{"acme/web": strings.TrimSpace(string(base))}
	st.PutCoverage(t.Context(), "e2e", []store.CoverageEntry{
		{Test: "t-cart", Files: []string{"acme/web:public/cart.js"}, Commits: commits},
		{Test: "t-pay", Files: []string{"acme/web:public/pay.js"}, Commits: commits},
	}, nil)
	t.Chdir(repo)

	out, code := realCLI(t, url, "run", "--affected", "--explain")
	want := `changed: 1 file
  acme/web:public/pay.js
1 of 2 tests affected
  tests/pay.spec.ts pay  because acme/web:public/pay.js
`
	if code != 0 || !strings.HasSuffix(out, want) {
		t.Fatalf("exit %d, output:\n%s\nwant it to end with:\n%s", code, out, want)
	}
	if !strings.Contains(out, "passed") {
		t.Fatalf("the explain run did not pass:\n%s", out)
	}
}

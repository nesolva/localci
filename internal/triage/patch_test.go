package triage

import (
	"context"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWith makes a git repository holding the given files and returns its directory and head.
func repoWith(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "init")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

const checkoutSpec = `import { test, expect } from '@playwright/test';

test('pays', async ({ page }) => {
  await page.goto('/checkout');
  await page.getByRole('button', { name: 'Pay' }).click();
  await expect(page.getByRole('heading', { name: 'Thank you' })).toBeVisible();
});
`

var payFix = Fix{Method: "getByRole", Role: "button", From: "Pay", To: "Place order"}

func TestBuildPatchChangesOnlyTheLocatorAndApplies(t *testing.T) {
	dir, head := repoWith(t, map[string]string{"e2e/checkout.spec.ts": checkoutSpec})
	loc := filepath.Join(dir, "e2e/checkout.spec.ts") + ":6"
	p, why := BuildPatch(dir, loc, []string{"checkout.spec.ts"}, payFix)
	if p == nil {
		t.Fatalf("no patch: %s", why)
	}
	if p.File != "e2e/checkout.spec.ts" || p.Line != 5 {
		t.Fatalf("patch = %+v", p)
	}
	patchFile := filepath.Join(t.TempDir(), "x.patch")
	os.WriteFile(patchFile, []byte(p.Diff), 0o644)
	info := PatchInfo{Path: patchFile, File: p.File, Shas: []string{head}, From: "Pay", To: "Place order"}
	if err := Apply(context.Background(), dir, info, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "e2e/checkout.spec.ts"))
	want := strings.Replace(checkoutSpec, "name: 'Pay'", `name: "Place order"`, 1)
	if string(got) != want {
		t.Fatalf("file after apply:\n%s", got)
	}
}

func TestBuildPatchRefusesPathsOutsideTheSuite(t *testing.T) {
	dir, _ := repoWith(t, map[string]string{
		"e2e/checkout.spec.ts": checkoutSpec,
		"src/app.ts":           "page.getByRole('button', { name: 'Pay' });\n",
	})
	outside := filepath.Join(filepath.Dir(dir), "x.ts")
	os.WriteFile(outside, []byte(checkoutSpec), 0o644)
	t.Cleanup(func() { os.Remove(outside) })
	// A link that leaves the workspace.
	os.Symlink(outside, filepath.Join(dir, "e2e/link.spec.ts"))

	for name, loc := range map[string]string{
		"dot dot":         "../../x.ts:6",
		"absolute out":    outside + ":6",
		"symlink out":     filepath.Join(dir, "e2e/link.spec.ts") + ":6",
		"not a test file": filepath.Join(dir, "src/app.ts") + ":1",
		"no line":         filepath.Join(dir, "e2e/checkout.spec.ts"),
		"missing file":    filepath.Join(dir, "e2e/none.spec.ts") + ":1",
	} {
		t.Run(name, func(t *testing.T) {
			p, why := BuildPatch(dir, loc, []string{"checkout.spec.ts", "link.spec.ts"}, payFix)
			if p != nil || !strings.HasPrefix(why, "no patch:") {
				t.Fatalf("patch = %v, why = %q", p, why)
			}
		})
	}
}

// The new locator comes from the page the code under test rendered: it must stay one string literal.
func TestBuildPatchEscapesHostileNames(t *testing.T) {
	hostile := []string{
		"Say \"hi\" and `go` ${process.exit(1)}",
		`back\slash'; await evil(); '`,
		"line\nbreak   sep",
		"</script>&amp;",
	}
	for i, name := range hostile {
		dir, _ := repoWith(t, map[string]string{"e2e/checkout.spec.ts": checkoutSpec})
		fix := payFix
		fix.To = name
		loc := filepath.Join(dir, "e2e/checkout.spec.ts") + ":6"
		p, why := BuildPatch(dir, loc, []string{"checkout.spec.ts"}, fix)
		if p == nil {
			t.Fatalf("%d: no patch: %s", i, why)
		}
		check := filepath.Join(t.TempDir(), "p.patch")
		os.WriteFile(check, []byte(p.Diff), 0o644)
		git(t, dir, "apply", "--check", check)
		git(t, dir, "apply", check)

		got, _ := os.ReadFile(filepath.Join(dir, "e2e/checkout.spec.ts"))
		line := strings.Split(string(got), "\n")[4]
		// The literal after `name: ` up to ` }` round-trips as a JSON string: same value, nothing more.
		start := strings.Index(line, "name: ") + len("name: ")
		end := strings.LastIndex(line, " })")
		var back string
		if err := json.Unmarshal([]byte(line[start:end]), &back); err != nil || back != name {
			t.Fatalf("%d: literal %q does not round-trip: %v %q", i, line[start:end], err, back)
		}
		if rest := line[end:]; rest != " }).click();" {
			t.Fatalf("%d: text after the literal changed: %q", i, rest)
		}
		if node, err := osexec.LookPath("node"); err == nil {
			mjs := filepath.Join(t.TempDir(), "check.mjs")
			os.WriteFile(mjs, []byte(strings.Replace(string(got), "import { test, expect } from '@playwright/test';", "const test = () => {}, expect = () => {};", 1)), 0o644)
			if out, err := osexec.Command(node, "--check", mjs).CombinedOutput(); err != nil {
				t.Fatalf("%d: node --check: %v\n%s", i, err, out)
			}
		}
	}
}

func TestBuildPatchKeepsLineEndings(t *testing.T) {
	crlf := strings.ReplaceAll(checkoutSpec, "\n", "\r\n")
	noEOL := strings.TrimSuffix(checkoutSpec, "\n")
	for name, content := range map[string]string{"crlf": crlf, "no trailing newline": noEOL} {
		t.Run(name, func(t *testing.T) {
			dir, _ := repoWith(t, map[string]string{"e2e/checkout.spec.ts": content})
			p, why := BuildPatch(dir, filepath.Join(dir, "e2e/checkout.spec.ts")+":6", []string{"checkout.spec.ts"}, payFix)
			if p == nil {
				t.Fatalf("no patch: %s", why)
			}
			f := filepath.Join(t.TempDir(), "p.patch")
			os.WriteFile(f, []byte(p.Diff), 0o644)
			git(t, dir, "apply", f)
			got, _ := os.ReadFile(filepath.Join(dir, "e2e/checkout.spec.ts"))
			if want := strings.Replace(content, "name: 'Pay'", `name: "Place order"`, 1); string(got) != want {
				t.Fatalf("file changed beyond the literal:\n%q", got)
			}
		})
	}
}

func TestBuildPatchSkipsALocatorThatIsNotThere(t *testing.T) {
	dir, _ := repoWith(t, map[string]string{"e2e/checkout.spec.ts": checkoutSpec})
	fix := payFix
	fix.From = "Missing"
	if p, why := BuildPatch(dir, filepath.Join(dir, "e2e/checkout.spec.ts")+":6", []string{"checkout.spec.ts"}, fix); p != nil || why == "" {
		t.Fatalf("patch = %v, why = %q", p, why)
	}
}

func TestApplyRefusals(t *testing.T) {
	dir, head := repoWith(t, map[string]string{"e2e/checkout.spec.ts": checkoutSpec})
	p, _ := BuildPatch(dir, filepath.Join(dir, "e2e/checkout.spec.ts")+":6", []string{"checkout.spec.ts"}, payFix)
	file := filepath.Join(t.TempDir(), "x.patch")
	os.WriteFile(file, []byte(p.Diff), 0o644)
	info := PatchInfo{Path: file, File: p.File, Shas: []string{head}}
	ctx := context.Background()

	t.Run("fork run", func(t *testing.T) {
		if err := Apply(ctx, dir, info, true); err == nil || !strings.Contains(err.Error(), "fork") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("different head", func(t *testing.T) {
		other := info
		other.Shas = []string{strings.Repeat("a", 40)}
		err := Apply(ctx, dir, other, false)
		if err == nil || !strings.Contains(err.Error(), "run tested aaaaaaaa") {
			t.Fatalf("err = %v", err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "e2e/checkout.spec.ts")); string(got) != checkoutSpec {
			t.Fatal("the file changed on a refusal")
		}
	})
	t.Run("a diff that touches another file", func(t *testing.T) {
		evil := filepath.Join(t.TempDir(), "evil.patch")
		os.WriteFile(evil, []byte(strings.Replace(p.Diff, "b/e2e/checkout.spec.ts", "b/.github/workflows/ci.yml", 1)), 0o644)
		bad := info
		bad.Path = evil
		if err := Apply(ctx, dir, bad, false); err == nil || !strings.Contains(err.Error(), "another file") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("the same head applies once", func(t *testing.T) {
		if err := Apply(ctx, dir, info, false); err != nil {
			t.Fatal(err)
		}
		if err := Apply(ctx, dir, info, false); err == nil {
			t.Fatal("applied twice")
		}
	})
}

// A copy of a test file under a dependency or build directory, or one git does not track, is never the suite's
// test: the failure's location is written by the code under test and may name any of them.
func TestApplyRefusesDependencyAndUntrackedCopies(t *testing.T) {
	dir, head := repoWith(t, map[string]string{
		"e2e/checkout.spec.ts":              checkoutSpec,
		"node_modules/pkg/checkout.spec.ts": checkoutSpec,
		"dist/checkout.spec.ts":             checkoutSpec,
	})
	untracked := filepath.Join(dir, "e2e/copy/checkout.spec.ts")
	os.MkdirAll(filepath.Dir(untracked), 0o755)
	os.WriteFile(untracked, []byte(checkoutSpec), 0o644)
	files := []string{"checkout.spec.ts"}

	for _, rel := range []string{"node_modules/pkg/checkout.spec.ts", "dist/checkout.spec.ts", "e2e/copy/checkout.spec.ts"} {
		t.Run("patch is not built for "+rel, func(t *testing.T) {
			p, why := BuildPatch(dir, filepath.Join(dir, rel)+":6", files, payFix)
			if p != nil || why == "" {
				t.Fatalf("patch = %+v, why = %q: a copy that is not the tracked test was patched", p, why)
			}
		})
		t.Run("a patch already written for "+rel+" is not applied", func(t *testing.T) {
			old := splitLines(checkoutSpec)
			changed := append([]string(nil), old...)
			changed[5] = strings.Replace(changed[5], "Pay", "Place order", 1)
			file := filepath.Join(t.TempDir(), "x.patch")
			os.WriteFile(file, []byte(unifiedDiff(rel, old, changed, 5)), 0o644)
			err := Apply(context.Background(), dir, PatchInfo{Path: file, File: rel, Shas: []string{head}}, false)
			if err == nil || !strings.Contains(err.Error(), "tracked") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

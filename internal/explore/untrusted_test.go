package explore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
func atoi(s string) int   { n, _ := strconv.Atoi(s); return n }

// samePath compares paths after resolving links (a temporary directory may itself sit behind one).
func samePath(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

// The findings file is written by an agent driving an app nobody reviewed: it can carry terminal escapes, endless
// text, and paths that point anywhere on the machine.
func TestAFindingWithEscapesAndAnEscapingScreenshotIsStoredPrintableAndWithoutThePath(t *testing.T) {
	out := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.png")
	os.WriteFile(secret, []byte("not yours"), 0o644)
	os.WriteFile(filepath.Join(out, "ok.png"), []byte("png"), 0o644)
	os.Symlink(secret, filepath.Join(out, "link.png"))
	os.Symlink(outside, filepath.Join(out, "dir"))

	file := filepath.Join(out, "findings.ndjson")
	lines := []string{
		`{"severity":"high","title":"\u001b[31mred\u001b[0m \u001b]0;pwned\u0007title\u202e","goal":"g\u0000oal","steps":["\u001b[2Jstep one","two"],"screenshot":"ok.png"}`,
		`{"severity":"critical!!","title":"parent dir","screenshot":"../` + filepath.Base(outside) + `/secret.png"}`,
		`{"severity":"low","title":"absolute path","screenshot":"` + secret + `"}`,
		`{"severity":"low","title":"through a link to a file","screenshot":"link.png"}`,
		`{"severity":"low","title":"through a link to a directory","screenshot":"dir/secret.png"}`,
		`{"severity":"low","title":"` + strings.Repeat("x", 5000) + `","steps":[` + strings.Repeat(`"s",`, 100) + `"s"]}`,
		`not json at all`,
		`{"severity":"low","title":"   "}`,
		`{"severity":"low","title":"cut off mid-wri`,
	}
	os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644)

	p, err := ReadFindings(file, out, "7", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Findings) != 6 || p.Skipped != 3 {
		t.Fatalf("stored %d, skipped %d", len(p.Findings), p.Skipped)
	}
	f := p.Findings[0]
	if f.Title != "red title" {
		t.Errorf("title = %q", f.Title)
	}
	for _, s := range append([]string{f.Title, f.Goal}, f.Steps...) {
		for _, r := range s {
			if unicode.IsControl(r) || r == '‮' {
				t.Errorf("%q still holds %U", s, r)
			}
		}
		if strings.Contains(s, "[31m") || strings.Contains(s, "pwned") || strings.Contains(s, "[2J") {
			t.Errorf("escape sequence left in %q", s)
		}
	}
	if !samePath(f.Screenshot, filepath.Join(out, "ok.png")) {
		t.Errorf("a screenshot inside the output directory is kept: %q", f.Screenshot)
	}
	for i, f := range p.Findings[1:5] {
		if f.Screenshot != "" {
			t.Errorf("finding %d kept the path %q", i+1, f.Screenshot)
		}
	}
	if p.Findings[1].Severity != "low" {
		t.Errorf("an unknown severity is %q", p.Findings[1].Severity)
	}
	big := p.Findings[5]
	if len([]rune(big.Title)) > MaxTitle || len(big.Steps) != MaxSteps {
		t.Errorf("title %d chars, %d steps", len([]rune(big.Title)), len(big.Steps))
	}
}

// A draft test is the agent's text saved for a person to read: it must land in the run's drafts directory even when
// the agent made that path a link.
func TestADraftTestIsNeverWrittenThroughALink(t *testing.T) {
	out := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(out, draftDirName))
	file := filepath.Join(out, "findings.ndjson")
	os.WriteFile(file, []byte(`{"severity":"low","title":"t","test":"test('x')"}`+"\n"), 0o644)
	p, _ := ReadFindings(file, out, "7", "e2e")
	if len(p.Findings) != 1 || p.Findings[0].Test != "" {
		t.Errorf("finding: %+v", p.Findings)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside the run: %v", ents)
	}
}

func TestTheAgentDoesNotInheritTheDaemonsSecretsInItsEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret")
	t.Setenv("ANTHROPIC_API_KEY", "model-key")
	g := newRig(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	if _, err := g.explorer(`env > `+dump, 5, SystemClock).Run(context.Background(), g.env); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dump)
	out := string(b)
	if strings.Contains(out, "GITHUB_TOKEN") || strings.Contains(out, "ghp_secret") {
		t.Errorf("the agent saw the daemon's GitHub token:\n%s", out)
	}
	if !strings.Contains(out, "ANTHROPIC_API_KEY=model-key") || !strings.Contains(out, "LOCALCI_FINDINGS=") {
		t.Errorf("the agent lost what it needs:\n%s", out)
	}
}

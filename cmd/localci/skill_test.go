package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/nesolva/localci/skill"
)

func TestSkillInstallWritesTheEmbeddedSkillToTheGivenDir(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"skill", "install", "--dir", dir}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got, err := os.ReadFile(filepath.Join(dir, "localci", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := skill.FS.ReadFile("localci/SKILL.md")
	if !bytes.Equal(got, want) {
		t.Error("the installed skill differs from the embedded one")
	}
	// Installing again over an existing copy works (an upgrade).
	if code := run([]string{"skill", "install", "--dir", dir}, &out, &errb); code != exitOK {
		t.Errorf("second install: exit %d: %s", code, errb.String())
	}
	if code := run([]string{"skill", "remove"}, &out, &errb); code != exitUsage {
		t.Errorf("unknown subcommand: exit %d", code)
	}
}

// The skill is what an agent reads; a command it names that does not exist sends the agent in circles.
func TestEveryCommandTheSkillNamesExists(t *testing.T) {
	body, _ := skill.FS.ReadFile("localci/SKILL.md")
	registered := map[string]bool{}
	for _, c := range commands {
		registered[c.name] = true
	}
	found := 0
	for _, m := range regexp.MustCompile("`localci ([a-z]+)").FindAllSubmatch(body, -1) {
		found++
		if name := string(m[1]); !registered[name] {
			t.Errorf("SKILL.md names `localci %s`, which is not a command", name)
		}
	}
	if found < 10 {
		t.Errorf("only %d commands found in SKILL.md: has the format changed?", found)
	}
}

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nesolva/localci/skill"
)

func init() {
	register("skill", "install the agent skill: skill install [--dir d]", false, cmdSkill)
}

func cmdSkill(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "install" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Fprintln(stderr, "usage: localci skill install [--dir d]   (default ~/.claude/skills)")
			return exitOK
		}
		fmt.Fprintln(stderr, "usage: localci skill install [--dir d]   (default ~/.claude/skills)")
		return exitUsage
	}
	fs := flag.NewFlagSet("localci skill install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "skills directory (default ~/.claude/skills)")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if *dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(stderr, "localci:", err)
			return exitInfra
		}
		*dir = filepath.Join(home, ".claude", "skills")
	}
	body, err := skill.FS.ReadFile("localci/SKILL.md")
	if err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	dest := filepath.Join(*dir, "localci", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	if err := os.WriteFile(dest, body, 0o644); err != nil {
		fmt.Fprintln(stderr, "localci:", err)
		return exitInfra
	}
	abs, _ := filepath.Abs(dest)
	fmt.Fprintf(stdout, "installed %s\n", abs)
	return exitOK
}

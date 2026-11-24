package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTokenWarningOnlyWhenTheServiceWouldNotFindTheToken(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	yes, no := func() bool { return true }, func() bool { return false }

	if w := tokenWarning(env(map[string]string{"GITHUB_TOKEN": "x"}), yes); w != "" {
		t.Errorf("gh is logged in, the service finds its token: %q", w)
	}
	if w := tokenWarning(env(nil), no); w != "" {
		t.Errorf("no token anywhere is a known state, not a surprise: %q", w)
	}
	if w := tokenWarning(env(map[string]string{"GITHUB_TOKEN": "x"}), no); !strings.Contains(w, "gh auth login") {
		t.Errorf("the warning must say how to fix it: %q", w)
	}
}

// gh prints GITHUB_TOKEN/GH_TOKEN from its environment, so asking it with the
// installing shell's environment hides that it has no login of its own.
func TestGhHasTokenIgnoresTheShellsTokenVariables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	dir := t.TempDir()
	fake := "#!/bin/sh\nt=\"${GH_TOKEN:-${GITHUB_TOKEN:-${GH_ENTERPRISE_TOKEN:-$GITHUB_ENTERPRISE_TOKEN}}}\"\n" +
		"[ -n \"$t\" ] && { echo \"$t\"; exit 0; }\necho 'not logged in' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("GITHUB_TOKEN", "from-shell")
	t.Setenv("GH_TOKEN", "from-shell")
	if ghHasToken() {
		t.Fatal("gh has no login of its own: the token comes from this shell only")
	}
	if w := tokenWarning(os.Getenv, ghHasToken); w == "" {
		t.Fatal("GITHUB_TOKEN only in this shell must warn")
	}
}

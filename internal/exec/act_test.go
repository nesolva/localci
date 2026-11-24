package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActPicksADefaultImageOnlyWithoutUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	if got := strings.Join(actImageArgs(), " "); !strings.Contains(got, "-P ubuntu-latest=") {
		t.Errorf("no config: want a default image, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, ".actrc"), []byte("-P ubuntu-latest=custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := actImageArgs(); got != nil {
		t.Errorf("user .actrc present: want no override, got %q", got)
	}
}

func TestResolveWorkflowByNameOrPath(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	os.MkdirAll(wf, 0o755)
	os.WriteFile(filepath.Join(wf, "lint.yml"), []byte("name: Lint\njobs:\n  a: {}\n"), 0o644)
	os.WriteFile(filepath.Join(wf, "deploy.yaml"), []byte("name: Deploy\njobs:\n  a: {}\n  b: {}\n"), 0o644)

	for _, c := range []struct{ workflow, job, want string }{
		{"Deploy", "b", ".github/workflows/deploy.yaml"},
		{"Lint", "a", ".github/workflows/lint.yml"},
		{".github/workflows/lint.yml", "a", ".github/workflows/lint.yml"},
	} {
		got, err := ResolveWorkflow(dir, c.workflow, c.job)
		if err != nil || got != c.want {
			t.Errorf("%s/%s = %q, %v; want %q", c.workflow, c.job, got, err, c.want)
		}
	}
	if _, err := ResolveWorkflow(dir, "Lint", "b"); err == nil {
		t.Error("a job the workflow does not have must not resolve")
	}
}

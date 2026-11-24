package exec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestToolchainAsImage(t *testing.T) {
	read := func(p string) ([]byte, error) {
		files := map[string]string{".nvmrc": "v20.11.0\n", "go.mod": "module acme/x\n\ngo 1.22.1\n", ".tool-versions": "nodejs 18.19.0\njava temurin-21\n"}
		if c, ok := files[p]; ok {
			return []byte(c), nil
		}
		return nil, os.ErrNotExist
	}
	cases := []struct {
		tools map[string]string
		want  string
	}{
		{map[string]string{"node": "20.x"}, "node:20"},
		{map[string]string{"node": "file:.nvmrc"}, "node:20.11.0"},
		{map[string]string{"node": "lts/*"}, "node:lts"},
		{map[string]string{"java": "21"}, "eclipse-temurin:21"},
		{map[string]string{"python": "3.12"}, "python:3.12"},
		{map[string]string{"go": "file:go.mod"}, "golang:1.22.1"},
		{map[string]string{"go": "^1.22"}, "golang:1.22"},
		{map[string]string{"node": "file:.tool-versions"}, "node:18.19.0"},
	}
	for _, c := range cases {
		tc, err := ResolveToolchain(context.Background(), read, c.tools, ToolchainContainers)
		if err != nil || tc.Image != c.want || tc.UsesMise() {
			t.Errorf("%v: image %q, err %v; want %q", c.tools, tc.Image, err, c.want)
		}
		if tc.Key() != "image:"+c.want {
			t.Errorf("%v: key %q", c.tools, tc.Key())
		}
	}
}

func TestToolchainErrors(t *testing.T) {
	read := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	for name, tools := range map[string]map[string]string{
		"two tools in one image":   {"node": "20", "java": "21"},
		"a version file not there": {"node": "file:.nvmrc"},
		"a tool nobody provides":   {"ruby": "3"},
	} {
		if _, err := ResolveToolchain(context.Background(), read, tools, ToolchainContainers); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if tc, err := ResolveToolchain(context.Background(), read, nil, ""); err != nil || tc.Key() != "" || tc.Wrap("x") != "x" {
		t.Errorf("no tools: %+v %v", tc, err)
	}
}

// With mise, the declared version is resolved the way mise would install it,
// and the command runs under `mise exec`.
func TestToolchainWithMise(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\n[ \"$1\" = latest ] && { echo \"${2#*@}.9\"; exit 0; }\n"
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	tc, err := ResolveToolchain(context.Background(), nil, map[string]string{"node": "20", "java": "21"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if tc.Key() != "mise:java@21.9,node@20.9" {
		t.Fatalf("key = %q", tc.Key())
	}
	want := fmt.Sprintf("mise exec %s %s -- bash -eo pipefail -c %s", "'java@temurin-21.9'", "'node@20.9'", "'node -v'")
	if got := tc.Wrap("node -v"); got != want {
		t.Fatalf("wrap = %q, want %q", got, want)
	}
}

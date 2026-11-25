package main

import (
	"runtime/debug"
	"testing"
)

func TestGoInstallVersionPrintsLikeARelease(t *testing.T) {
	cases := []struct {
		stamped, module, want string
	}{
		{"0.1.0", "v0.1.0", "0.1.0"}, // release build: the stamp wins
		{"dev", "v0.1.0", "0.1.0"},   // go install …@v0.1.0
		{"dev", "(devel)", "dev"},    // go build from a checkout
		{"dev", "", "dev"},
	}
	for _, c := range cases {
		info := &debug.BuildInfo{Main: debug.Module{Version: c.module}}
		if got := pickVersion(c.stamped, info); got != c.want {
			t.Errorf("pickVersion(%q, %q) = %q, want %q", c.stamped, c.module, got, c.want)
		}
	}
	if got := pickVersion("dev", nil); got != "dev" {
		t.Errorf("no build info: got %q, want dev", got)
	}
}

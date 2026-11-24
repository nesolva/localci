package main

import (
	"fmt"
	"io"
	"runtime/debug"
)

// versionString returns the release version: the value injected at build
// time, else the module version recorded by `go install …@vX.Y.Z`, else "dev".
func versionString() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return version
}

func init() {
	register("version", "print the version", false, func(_ []string, stdout, _ io.Writer) int {
		fmt.Fprintln(stdout, "localci", versionString())
		return exitOK
	})
}

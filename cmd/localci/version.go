package main

import (
	"fmt"
	"io"
	"runtime/debug"
	"strings"
)

// versionString returns the release version: the value injected at build
// time, else the module version recorded by `go install …@vX.Y.Z`, else "dev".
func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return pickVersion(version, info)
}

// pickVersion prints a `go install` build like a release build: "0.1.0",
// not the module tag "v0.1.0".
func pickVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "dev" {
		return stamped
	}
	if info != nil {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return stamped
}

func init() {
	register("version", "print the version", false, func(_ []string, stdout, _ io.Writer) int {
		fmt.Fprintln(stdout, "localci", versionString())
		return exitOK
	})
}

// Command gen writes docs/config.md from the config schema.
package main

import (
	"fmt"
	"os"

	"github.com/nesolva/localci/internal/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen <output.md>")
		os.Exit(2)
	}
	if err := os.WriteFile(os.Args[1], []byte(config.Markdown()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

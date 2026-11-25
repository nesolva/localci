package suite_test

import (
	"path/filepath"
	"testing"

	"github.com/nesolva/localci/internal/config"
	"github.com/nesolva/localci/internal/suite"
)

// Users start from the shipped examples: each one loads as it is, through
// the same configuration and suite parsing the server uses.
func TestExamplesLoad(t *testing.T) {
	files, err := filepath.Glob("../../examples/*/localci.yaml")
	if err != nil || len(files) < 5 {
		t.Fatalf("want at least 5 examples, got %d (%v)", len(files), err)
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f)), func(t *testing.T) {
			cfg, err := config.Load(config.Options{Path: f, Env: func(string) string { return "" }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := suite.FromConfig(cfg.Suites); err != nil {
				t.Fatal(err)
			}
		})
	}
}

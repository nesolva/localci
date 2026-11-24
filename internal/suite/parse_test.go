package suite

import (
	"strings"
	"testing"
)

// localci/checks is the status that says whether GitHub's merge ref was ready
// for a pull request: a suite of that name would share its status context.
func TestASuiteCannotBeNamedChecks(t *testing.T) {
	for _, name := range []string{"checks", "Checks"} {
		_, err := ParseYAML([]byte("suites:\n  " + name + ":\n    repos: [acme/shop]\n    test: {repo: acme/shop, run: make e2e}\n"))
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("suite %q: err = %v, want it rejected as a reserved name", name, err)
		}
	}
	if _, err := ParseYAML([]byte("suites:\n  checkout:\n    repos: [acme/shop]\n    test: {repo: acme/shop, run: make e2e}\n")); err != nil {
		t.Fatalf("suite checkout: %v", err)
	}
}

func TestDefaultListNeverFetchesFromTheRegistry(t *testing.T) {
	// An unscoped name on the registry is not ours: npx must only use the installed package.
	if !strings.Contains(DefaultList, "--no-install") && !strings.Contains(DefaultList, "@nesolva/") {
		t.Fatalf("default list command %q may download an unscoped package", DefaultList)
	}
}

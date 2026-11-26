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

// A port belongs to a shard and the database template is built outside any: a port in the seed or in the
// database URL is refused when the configuration is read, naming the key.
func TestDatabaseTemplatesRefuseAShardPort(t *testing.T) {
	for key, yaml := range map[string]string{
		"database.template.seed": strings.Replace(fakeSuite(""), `seed: "echo seeded > {{db.url}}"`,
			`seed: 'echo {{port "db"}} > {{db.url}}'`, 1),
		"database.url": strings.Replace(fakeSuite(""), "driver: command\n",
			"driver: command\n      url: '/data/{{port \"db\"}}/{{db.name}}'\n", 1),
	} {
		_, err := ParseYAML([]byte(yaml))
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "port") {
			t.Errorf("%s with {{port}}: err = %v, want it refused naming the key", key, err)
		}
	}
}

func TestDefaultListNeverFetchesFromTheRegistry(t *testing.T) {
	// An unscoped name on the registry is not ours: npx must only use the installed package.
	if !strings.Contains(DefaultList, "--no-install") && !strings.Contains(DefaultList, "@nesolva/") {
		t.Fatalf("default list command %q may download an unscoped package", DefaultList)
	}
}

// The stack and the seed reach a Postgres database through its URL; a
// container only runs the tools. A postgres database without url is refused
// when the configuration is read, naming the key.
func TestPostgresDatabaseNeedsAURL(t *testing.T) {
	suite := func(db string) string {
		return "suites:\n  e2e:\n    repos: [acme/shop]\n" +
			"    database: {driver: postgres, " + db + "template: {repo: acme/shop, seed: make seed}}\n" +
			"    test: {repo: acme/shop, run: make e2e}\n"
	}
	for _, db := range []string{"container: acme-postgres, ", ""} {
		_, err := ParseYAML([]byte(suite(db)))
		if err == nil || !strings.Contains(err.Error(), "database.url") {
			t.Errorf("postgres with %q: err = %v, want it refused naming database.url", db, err)
		}
	}
	if _, err := ParseYAML([]byte(suite("url: 'postgres://postgres@127.0.0.1:5432/postgres', container: acme-postgres, "))); err != nil {
		t.Errorf("postgres with url and container: %v", err)
	}
}

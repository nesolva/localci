// Package suite runs end-to-end suites on this machine: it checks the repos out, prepares them only when needed,
// clones a database and starts a stack per shard, splits the tests by measured duration, streams the results into
// the store, moves conflicting tests to the serial group and tears everything down.
package suite

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nesolva/localci/internal/config"
	"gopkg.in/yaml.v3"
)

// Suite is one entry of `suites:` in the configuration.
type Suite struct {
	Name      string
	Repos     []string
	Trigger   Trigger
	Prepare   []Prepare
	Database  *Database
	Stack     []Service
	Test      Test
	Shards    int // 0 = auto
	Isolation string
	Timeout   time.Duration // 0 = none
	Exclusive bool

	// Raw is the suite's whole mapping node, so packages of later phases read their own keys from it without
	// this package knowing them.
	Raw *yaml.Node
}

// Trigger says what starts the suite for a pull request.
type Trigger struct {
	Push  bool   // every new head
	Label string // a label added to the pull request
}

// Prepare is a command run once per run in the warm workspaces, skipped while the hash of CacheKey is unchanged.
type Prepare struct {
	Repo     string
	Run      string
	CacheKey []string // glob patterns relative to the repo; the command itself is part of the key
	Keep     []string // paths the command produces that survive the next checkout
}

// Database says how every shard gets its own database.
type Database struct {
	Driver   string // command | postgres
	Template Template
	URL      string // {{db.url}}; defaults to the connection URL (postgres) or the database file (command)

	// command driver: {{template}} and {{name}} are replaced by the driver.
	Create   string
	Drop     string
	Snapshot string

	// postgres driver
	Container string
	User      string
}

// Template builds the database every shard database is cloned from.
type Template struct {
	Seed string   // command that fills the empty template; may use {{db.name}} and {{db.url}}
	Key  []string // "repo:glob": the files whose content decides when the template is rebuilt
	Repo string   // where Seed runs; default: the repo of the first key
}

// Service is one process of the stack every shard gets.
type Service struct {
	Name  string
	Repo  string
	Run   string
	Env   map[string]string
	Ready Ready
}

// Ready says when a service takes requests; exactly one of HTTP, TCP, Log is set.
type Ready struct {
	HTTP    string
	TCP     string // a port, or a template such as {{port "db"}}
	Log     string // regular expression
	Timeout time.Duration
}

// Test is the command that runs the tests of one shard.
type Test struct {
	Repo string
	Run  string
	// List prints one JSON object per line ({"test","file","title","line"}) for every test of the suite.
	List string
	Env  map[string]string
}

// DefaultList is how tests are listed when the suite does not say.
const DefaultList = "npx --no-install --offline localci-playwright list"

type rawSuite struct {
	Repos   []string `yaml:"repos"`
	Trigger struct {
		Push  bool   `yaml:"push"`
		Label string `yaml:"label"`
	} `yaml:"trigger"`
	Prepare []struct {
		Repo     string   `yaml:"repo"`
		Run      string   `yaml:"run"`
		CacheKey []string `yaml:"cache_key"`
		Keep     []string `yaml:"keep"`
	} `yaml:"prepare"`
	Database *struct {
		Driver   string `yaml:"driver"`
		Template struct {
			Seed string   `yaml:"seed"`
			Key  []string `yaml:"key"`
			Repo string   `yaml:"repo"`
		} `yaml:"template"`
		URL       string `yaml:"url"`
		Create    string `yaml:"create"`
		Drop      string `yaml:"drop"`
		Snapshot  string `yaml:"snapshot"`
		Container string `yaml:"container"`
		User      string `yaml:"user"`
	} `yaml:"database"`
	Stack []struct {
		Name  string            `yaml:"name"`
		Repo  string            `yaml:"repo"`
		Run   string            `yaml:"run"`
		Env   map[string]string `yaml:"env"`
		Ready struct {
			HTTP    string        `yaml:"http"`
			TCP     any           `yaml:"tcp"`
			Log     string        `yaml:"log"`
			Timeout time.Duration `yaml:"timeout"`
		} `yaml:"ready"`
	} `yaml:"stack"`
	Test struct {
		Repo string            `yaml:"repo"`
		Run  string            `yaml:"run"`
		List string            `yaml:"list"`
		Env  map[string]string `yaml:"env"`
	} `yaml:"test"`
	Shards    any           `yaml:"shards"`
	Isolation string        `yaml:"isolation"`
	Timeout   time.Duration `yaml:"timeout"`
	Exclusive bool          `yaml:"exclusive"`
}

// Parse reads one suite from its mapping node. Keys this package does not know are left alone (see Suite.Raw).
func Parse(name string, n *yaml.Node) (*Suite, error) {
	fail := func(format string, args ...any) (*Suite, error) {
		return nil, fmt.Errorf("suites.%s: %s", name, fmt.Sprintf(format, args...))
	}
	if strings.EqualFold(name, "checks") {
		// Its status context, localci/checks, says whether GitHub's merge ref
		// was ready for a pull request's workflow checks.
		return fail("the name checks is reserved (its status, localci/checks, belongs to workflow checks); pick another name")
	}
	if err := config.CheckSuiteName(name); err != nil {
		return fail("%v", err)
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return fail("expected a mapping")
	}
	var r rawSuite
	if err := n.Decode(&r); err != nil {
		return fail("%v", err)
	}
	s := &Suite{Name: name, Repos: r.Repos, Isolation: r.Isolation, Timeout: r.Timeout, Exclusive: r.Exclusive, Raw: n}
	s.Trigger = Trigger{Push: r.Trigger.Push, Label: r.Trigger.Label}
	if len(s.Repos) == 0 {
		return fail("repos: list at least one repository")
	}
	known := map[string]bool{}
	for _, repo := range s.Repos {
		known[repo] = true
	}
	inRepos := func(key, repo string) error {
		if !known[repo] {
			return fmt.Errorf("%s: repository %q is not in repos", key, repo)
		}
		return nil
	}

	switch r.Isolation {
	case "", "isolated":
		s.Isolation = "isolated"
	case "auto", "shared-db", "tenant":
		s.Isolation = r.Isolation
	default:
		return fail("isolation: %q is not known (use isolated, shared-db, tenant or auto)", r.Isolation)
	}
	switch v := r.Shards.(type) {
	case nil:
	case int:
		if v < 1 {
			return fail("shards: use auto or a number of at least 1")
		}
		s.Shards = v
	case string:
		if v != "auto" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fail("shards: use auto or a number of at least 1")
			}
			s.Shards = n
		}
	default:
		return fail("shards: use auto or a number")
	}

	for i, p := range r.Prepare {
		key := fmt.Sprintf("prepare[%d]", i)
		if p.Run == "" {
			return fail("%s.run: a command is required", key)
		}
		if p.Repo == "" {
			p.Repo = s.Repos[0]
		}
		if err := inRepos(key+".repo", p.Repo); err != nil {
			return fail("%v", err)
		}
		s.Prepare = append(s.Prepare, Prepare{Repo: p.Repo, Run: p.Run, CacheKey: p.CacheKey, Keep: p.Keep})
	}

	if d := r.Database; d != nil {
		db := &Database{Driver: d.Driver, URL: d.URL, Create: d.Create, Drop: d.Drop, Snapshot: d.Snapshot,
			Container: d.Container, User: d.User,
			Template: Template{Seed: d.Template.Seed, Key: d.Template.Key, Repo: d.Template.Repo}}
		switch db.Driver {
		case "command":
			if db.Create == "" || db.Drop == "" {
				return fail("database: the command driver needs create and drop commands")
			}
		case "postgres":
			// The seed and the stack reach the database through its URL; a
			// container only runs createdb and psql.
			if db.URL == "" {
				return fail("database.url: the postgres driver needs the URL the seed and the stack connect to, like postgres://postgres:postgres@127.0.0.1:5432/postgres")
			}
		default:
			return fail("database.driver: %q is not known (use postgres or command)", db.Driver)
		}
		if db.Template.Seed == "" {
			return fail("database.template.seed: a command is required")
		}
		for _, k := range db.Template.Key {
			repo, _, ok := strings.Cut(k, ":")
			if !ok {
				return fail("database.template.key: %q must look like repo:glob", k)
			}
			if err := inRepos("database.template.key", repo); err != nil {
				return fail("%v", err)
			}
		}
		if db.Template.Repo == "" && len(db.Template.Key) > 0 {
			db.Template.Repo, _, _ = strings.Cut(db.Template.Key[0], ":")
		}
		if db.Template.Repo == "" {
			db.Template.Repo = s.Repos[0]
		}
		if err := inRepos("database.template.repo", db.Template.Repo); err != nil {
			return fail("%v", err)
		}
		s.Database = db
	}
	if err := checkIsolationKeys(s); err != nil {
		return fail("%v", err)
	}

	names := map[string]bool{}
	for i, sv := range r.Stack {
		key := fmt.Sprintf("stack[%d]", i)
		if sv.Name == "" || sv.Run == "" {
			return fail("%s: name and run are required", key)
		}
		if names[sv.Name] {
			return fail("%s: the name %q is used twice", key, sv.Name)
		}
		names[sv.Name] = true
		if sv.Repo == "" {
			sv.Repo = s.Repos[0]
		}
		if err := inRepos(key+".repo", sv.Repo); err != nil {
			return fail("%v", err)
		}
		ready := Ready{HTTP: sv.Ready.HTTP, Log: sv.Ready.Log, Timeout: sv.Ready.Timeout}
		if sv.Ready.TCP != nil {
			ready.TCP = fmt.Sprint(sv.Ready.TCP)
		}
		set := 0
		for _, v := range []string{ready.HTTP, ready.TCP, ready.Log} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return fail("%s.ready: set exactly one of http, tcp, log", key)
		}
		s.Stack = append(s.Stack, Service{Name: sv.Name, Repo: sv.Repo, Run: sv.Run, Env: sv.Env, Ready: ready})
	}

	s.Test = Test{Repo: r.Test.Repo, Run: r.Test.Run, List: r.Test.List, Env: r.Test.Env}
	if s.Test.Run == "" {
		return fail("test.run: a command is required")
	}
	if s.Test.Repo == "" {
		s.Test.Repo = s.Repos[0]
	}
	if err := inRepos("test.repo", s.Test.Repo); err != nil {
		return fail("%v", err)
	}
	if s.Test.List == "" {
		s.Test.List = DefaultList
	}

	if err := s.checkTemplates(); err != nil {
		return fail("%v", err)
	}
	if err := checkTenantKeys(s); err != nil {
		return fail("%v", err)
	}
	return s, nil
}

// FromConfig parses every entry of config's `suites` mapping, in name order.
func FromConfig(m map[string]any) ([]*Suite, error) {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*Suite
	for _, name := range names {
		b, err := yaml.Marshal(m[name])
		if err != nil {
			return nil, fmt.Errorf("suites.%s: %w", name, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil || len(doc.Content) == 0 {
			return nil, fmt.Errorf("suites.%s: %v", name, err)
		}
		s, err := Parse(name, doc.Content[0])
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ParseYAML reads a document with a top-level `suites:` mapping (handy for tests and tools).
func ParseYAML(data []byte) ([]*Suite, error) {
	var doc struct {
		Suites map[string]any `yaml:"suites"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return FromConfig(doc.Suites)
}

// Repo returns the position of repo in the suite, or -1.
func (s *Suite) hasRepo(repo string) bool {
	for _, r := range s.Repos {
		if r == repo {
			return true
		}
	}
	return false
}

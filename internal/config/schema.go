package config

import "time"

// Source says where an effective value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceAuto    Source = "auto"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

// Repo is one watched repository.
type Repo struct {
	Name          string   `json:"name"`
	SkipWorkflows []string `json:"skip_workflows,omitempty"`
	// Checks runs the repository's pull request checks (`.github/workflows`); default true.
	Checks bool `json:"checks"`
	// Path is the local checkout that is this repository, whatever its `origin` says (resolved, absolute).
	Path string `json:"path,omitempty"`
}

// Config is the effective configuration, schema v1.
type Config struct {
	Version int
	Repos   []Repo
	Watch   struct {
		Poll   time.Duration
		MaxAge time.Duration
		Drafts bool
		Forks  string
	}
	Capacity struct {
		CPUs         int
		Memory       uint64 // bytes
		DockerMemory uint64 // bytes, 0 = no Docker
		CheckSlots   int
		JobSlots     int
	}
	Fidelity struct {
		MergeRef    bool
		PathFilters bool
		Toolchains  string // mise | containers (auto is resolved)
		DockerCache bool
	}
	Suites map[string]any
	// Explore is the `explore` section, parsed by internal/explore (the nightly exploratory agent).
	Explore map[string]any
	Notify  struct {
		On []string
	}
	Server struct {
		Port       int
		Listen     string // local | lan: where the worker listener binds
		WorkerPort int
	}
	Workers struct {
		Forks bool // let pull requests from forks run on a worker
	}
}

type kind int

const (
	kInt kind = iota
	kBool
	kDuration
	kEnum
	kSize
	kList
	kRepos
	kMap
	kSection // a mapping whose keys belong to the package that owns the section
)

// Field is one configuration key. The registry below is the single source for
// loading, validation, env/flag names and the generated docs/config.md.
type Field struct {
	Key     string // dotted path, e.g. "watch.poll"
	Kind    kind
	Default string // shown in docs and used when nothing sets it
	Auto    bool   // accepts "auto": resolved from the machine
	Enum    []string
	Min     int64 // for ints, seconds for durations, 0 = no minimum
	Max     int64 // for ints, 0 = no maximum
	Doc     string
	ref     func(*Config) any // pointer to the typed field
	noEnv   bool              // no env var or flag (structured values)
}

var allNotify = []string{"failed", "passed"}

// Fields lists every key in documentation order.
var Fields = []Field{
	{Key: "version", Kind: kInt, Default: "1", Min: 1, Max: 1, noEnv: true,
		Doc: "Schema version. Only 1 exists.",
		ref: func(c *Config) any { return &c.Version }},
	{Key: "repos", Kind: kRepos, Default: "(none)",
		Doc: "Repositories to watch, as `owner/name` entries; each may set `workflows.skip` (workflow files to ignore) and `checks: false` (watch it for suites only, run none of its workflow checks). " +
			"`path` maps a repository to its checkout on this machine (`~` allowed, must exist): `localci run` from that directory tests it as this repository whatever `origin` says, so a repository with no `origin` works (suites clone it from that directory, not from GitHub); " +
			"a repository with `path` and `checks: false` is not polled for pull requests, so set both for one that lives only here. " +
			"Required to run checks. Env and flag take a comma-separated list of names, which replaces the whole list (so `path` is set in the file only).",
		ref: func(c *Config) any { return &c.Repos }},
	{Key: "watch.poll", Kind: kDuration, Default: "60s", Min: 10,
		Doc: "How often to look for new or updated pull requests (at least 10s).",
		ref: func(c *Config) any { return &c.Watch.Poll }},
	{Key: "watch.max_age", Kind: kDuration, Default: "24h", Min: 1,
		Doc: "Pull request heads older than this only run when submitted by hand.",
		ref: func(c *Config) any { return &c.Watch.MaxAge }},
	{Key: "watch.drafts", Kind: kBool, Default: "false",
		Doc: "Also run checks on draft pull requests.",
		ref: func(c *Config) any { return &c.Watch.Drafts }},
	{Key: "watch.forks", Kind: kEnum, Default: "label", Enum: []string{"never", "label", "always"},
		Doc: "Pull requests from forks: `never` runs none, `label` runs one while it has the `ok-to-test` label and someone with write access has approved its latest commit in a review (each new push needs a new approval; removing the label stops it) or its author has write access and the pull request comes from the author's own fork (GitHub does not let authors approve their own pull request; a pull request from someone else's fork always needs the approval). Remaining risk: anyone the author added as a collaborator on their own fork can push to it, and those pushes run without a review. `always` runs all.",
		ref: func(c *Config) any { return &c.Watch.Forks }},
	{Key: "capacity.cpus", Kind: kInt, Default: "auto", Auto: true, Min: 1,
		Doc: "CPUs localci may use. `auto` is the machine's logical CPUs.",
		ref: func(c *Config) any { return &c.Capacity.CPUs }},
	{Key: "capacity.memory", Kind: kSize, Default: "auto", Auto: true,
		Doc: "Memory localci may use, such as `8GiB`. `auto` is the memory free at startup.",
		ref: func(c *Config) any { return &c.Capacity.Memory }},
	{Key: "capacity.docker_memory", Kind: kSize, Default: "auto", Auto: true,
		Doc: "Memory available to Docker, such as `6GiB`. `auto` asks `docker info`; 0 when Docker is not running.",
		ref: func(c *Config) any { return &c.Capacity.DockerMemory }},
	{Key: "capacity.check_slots", Kind: kInt, Default: "auto", Auto: true, Min: 1, Max: 64,
		Doc: "Checks running at once. `auto` follows CPUs, memory and Docker memory.",
		ref: func(c *Config) any { return &c.Capacity.CheckSlots }},
	{Key: "capacity.job_slots", Kind: kInt, Default: "auto", Auto: true, Min: 1, Max: 64,
		Doc: "Pull request runs in progress at once. `auto` is half the check slots, at least 1.",
		ref: func(c *Config) any { return &c.Capacity.JobSlots }},
	{Key: "fidelity.merge_ref", Kind: kBool, Default: "true",
		Doc: "Test the pull request's merge result (`refs/pull/N/merge`) instead of its head.",
		ref: func(c *Config) any { return &c.Fidelity.MergeRef }},
	{Key: "fidelity.path_filters", Kind: kBool, Default: "true",
		Doc: "Honour a workflow's `paths` and `paths-ignore`.",
		ref: func(c *Config) any { return &c.Fidelity.PathFilters }},
	{Key: "fidelity.toolchains", Kind: kEnum, Default: "auto", Auto: true, Enum: []string{"mise", "containers"},
		Doc: "How `setup-*` tool versions are provided. `auto` uses `mise` when installed, else containers.",
		ref: func(c *Config) any { return &c.Fidelity.Toolchains }},
	{Key: "fidelity.docker_cache", Kind: kBool, Default: "true",
		Doc: "Give the `docker run` lines of a check a cache volume that survives from one run to the next.",
		ref: func(c *Config) any { return &c.Fidelity.DockerCache }},
	{Key: "suites", Kind: kMap, Default: "{}", noEnv: true,
		Doc: "E2E suites, each a mapping under its name (lowercase letters, digits and `-`): `repos`, `trigger`, `prepare`, `database`, `stack`, `test`, `shards` (a number or `auto`), `isolation`, `timeout`. See docs/suites.md.",
		ref: func(c *Config) any { return &c.Suites }},
	{Key: "explore", Kind: kSection, Default: "(off)", noEnv: true,
		Doc: "The nightly exploratory agent: `schedule` (cron, five fields), `suite` (the suite whose stack it explores), `base_url` (where the app listens), `goals` (what it tries, in your words), `agent.command`, `agent.minutes` and `agent.env`. Off when the section is absent; when present it needs all of `schedule`, `suite`, `base_url` and at least one goal. See docs/explore.md.",
		ref: func(c *Config) any { return &c.Explore }},
	{Key: "notify.on", Kind: kList, Default: "failed, passed", Enum: allNotify,
		Doc: "Which outcomes of a run you started by hand (`localci run`, a retry) raise a desktop notification: any of `failed`, `passed`. Cancelled runs never do. Env and flag take a comma-separated list.",
		ref: func(c *Config) any { return &c.Notify.On }},
	{Key: "server.port", Kind: kInt, Default: "7357", Min: 1, Max: 65535,
		Doc: "Port of the dashboard (127.0.0.1 only). The CLI uses the API socket in the data directory, not this port.",
		ref: func(c *Config) any { return &c.Server.Port }},
	{Key: "server.listen", Kind: kEnum, Default: "local", Enum: []string{"local", "lan"},
		Doc: "Where the worker listener binds: `local` is 127.0.0.1 only, `lan` is every network interface so another machine can join as a worker. The dashboard and the API socket never leave this machine.",
		ref: func(c *Config) any { return &c.Server.Listen }},
	{Key: "server.worker_port", Kind: kInt, Default: "7358", Min: 1, Max: 65535,
		Doc: "Port of the worker listener (TLS, worker credentials only).",
		ref: func(c *Config) any { return &c.Server.WorkerPort }},
	{Key: "workers.forks", Kind: kBool, Default: "false",
		Doc: "Let runs of pull requests from forks use workers. Off by default: a fork's code would run on another machine.",
		ref: func(c *Config) any { return &c.Workers.Forks }},
}

// EnvNames lists the environment variables that override a configuration key.
func EnvNames() []string {
	var out []string
	for _, f := range Fields {
		if !f.noEnv {
			out = append(out, EnvName(f.Key))
		}
	}
	return out
}

func fieldByKey(key string) *Field {
	for i := range Fields {
		if Fields[i].Key == key {
			return &Fields[i]
		}
	}
	return nil
}

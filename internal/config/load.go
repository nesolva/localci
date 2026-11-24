// Package config loads localci's configuration from defaults, the config file,
// LOCALCI_* environment variables and command-line flags (later layers win),
// and remembers where every value came from.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nesolva/localci/internal/capacity"
)

// Options controls one Load.
type Options struct {
	// Path is an explicit config file; empty means LOCALCI_CONFIG, then the default location.
	// An explicit file that does not exist is an error; a missing default file is not.
	Path string
	// Env looks up an environment variable; nil means os.Getenv.
	Env func(string) string
	// Flags holds the raw value of each flag the user set, keyed by config key.
	Flags map[string]string
	// Machine supplies what "auto" resolves against.
	Machine capacity.Machine
}

// Effective is the loaded configuration plus where each value came from.
type Effective struct {
	Config
	File      string            // config file consulted
	FileFound bool              // false when the default file does not exist
	Sources   map[string]Source // by dotted key
}

// EnvName is the environment variable that overrides a key.
func EnvName(key string) string {
	return "LOCALCI_" + strings.ToUpper(strings.NewReplacer(".", "_").Replace(key))
}

// FlagName is the command-line flag that overrides a key.
func FlagName(key string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(key)
}

// Load builds the effective configuration. It returns Errors listing every
// invalid value with its file and line (or env var / flag name).
func Load(o Options) (*Effective, error) {
	if o.Env == nil {
		o.Env = os.Getenv
	}
	l := &loader{o: o, auto: map[string]bool{}, src: map[string]Source{}}
	l.defaults()

	explicit := o.Path != ""
	path := o.Path
	if path == "" {
		path = o.Env("LOCALCI_CONFIG")
		explicit = path != ""
	}
	if path == "" {
		path = ConfigPath()
	}
	l.file = path
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		l.found = true
		l.applyFile(data)
	case errors.Is(err, fs.ErrNotExist) && !explicit:
	default:
		return nil, Errors{{File: path, Msg: err.Error()}}
	}

	l.applyLayer(SourceEnv, func(f *Field) (string, string, bool) {
		name := EnvName(f.Key)
		v := o.Env(name)
		return v, "env " + name, v != ""
	})
	l.applyLayer(SourceFlag, func(f *Field) (string, string, bool) {
		v, ok := o.Flags[f.Key]
		return v, "flag --" + FlagName(f.Key), ok
	})
	if len(l.errs) > 0 {
		return nil, l.errs
	}
	l.resolveAuto()
	return &Effective{Config: l.cfg, File: path, FileFound: l.found, Sources: l.src}, nil
}

// Validate checks what a running service needs but inspection does not.
func (e *Effective) Validate() error {
	if len(e.Repos) == 0 {
		return Errors{{File: e.File, Field: "repos", Msg: "required: list at least one repository (owner/name)"}}
	}
	return nil
}

type loader struct {
	o     Options
	cfg   Config
	file  string
	found bool
	auto  map[string]bool
	src   map[string]Source
	errs  Errors
}

func (l *loader) defaults() {
	for i := range Fields {
		f := &Fields[i]
		l.src[f.Key] = SourceDefault
		switch {
		case f.Auto:
			l.auto[f.Key] = true
			l.src[f.Key] = SourceAuto
		case f.Kind == kMap:
			l.cfg.Suites = map[string]any{}
		case f.Kind == kRepos, f.Kind == kSection:
		default:
			if _, err := applyString(&l.cfg, f, strings.NewReplacer(", ", ",").Replace(f.Default)); err != nil {
				panic(fmt.Sprintf("config: bad default for %s: %v", f.Key, err))
			}
		}
	}
}

func (l *loader) applyLayer(src Source, get func(*Field) (raw, where string, ok bool)) {
	for i := range Fields {
		f := &Fields[i]
		if f.noEnv {
			continue
		}
		raw, where, ok := get(f)
		if !ok {
			continue
		}
		auto, err := applyString(&l.cfg, f, raw)
		if err != nil {
			l.errs = append(l.errs, &Error{File: where, Msg: err.Error()})
			continue
		}
		l.set(f, src, auto)
	}
}

func (l *loader) set(f *Field, src Source, auto bool) {
	l.auto[f.Key] = auto
	if auto {
		l.src[f.Key] = SourceAuto
	} else {
		l.src[f.Key] = src
	}
}

// resolveAuto fills "auto" values from the machine. Slots follow the effective
// CPU and memory budgets, including ones the user set by hand.
func (l *loader) resolveAuto() {
	m, c := l.o.Machine, &l.cfg
	if l.auto["capacity.cpus"] {
		c.Capacity.CPUs = m.CPUs
	}
	if l.auto["capacity.memory"] {
		c.Capacity.Memory = m.FreeMemory
	}
	if l.auto["capacity.docker_memory"] {
		c.Capacity.DockerMemory = m.DockerMemory
	}
	check, _ := capacity.Slots(c.Capacity.CPUs, c.Capacity.Memory, c.Capacity.DockerMemory)
	if l.auto["capacity.check_slots"] {
		c.Capacity.CheckSlots = check
	}
	if l.auto["capacity.job_slots"] {
		c.Capacity.JobSlots = capacity.JobSlots(c.Capacity.CheckSlots)
	}
	if l.auto["fidelity.toolchains"] {
		c.Fidelity.Toolchains = "containers"
		if m.Mise {
			c.Fidelity.Toolchains = "mise"
		}
	}
}

// ---- config file ----

func (l *loader) fileErr(n *yaml.Node, field, format string, args ...any) {
	l.errs = append(l.errs, &Error{File: l.file, Line: n.Line, Field: field, Msg: fmt.Sprintf(format, args...)})
}

func (l *loader) applyFile(data []byte) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		line := 0
		fmt.Sscanf(strings.TrimPrefix(err.Error(), "yaml: line "), "%d", &line)
		l.errs = append(l.errs, &Error{File: l.file, Line: line, Msg: "invalid YAML: " + strings.TrimPrefix(err.Error(), "yaml: ")})
		return
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return // empty file
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return
	}
	if root.Kind != yaml.MappingNode {
		l.fileErr(root, "", "expected a mapping of settings at the top level")
		return
	}
	var topKeys, sections []string
	seenSection := map[string]bool{}
	for _, f := range Fields {
		sec, _, nested := strings.Cut(f.Key, ".")
		if nested {
			if !seenSection[sec] {
				seenSection[sec] = true
				sections = append(sections, sec)
				topKeys = append(topKeys, sec)
			}
		} else {
			topKeys = append(topKeys, f.Key)
		}
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if seen[k.Value] {
			l.fileErr(k, k.Value, "duplicate key")
			continue
		}
		seen[k.Value] = true
		if f := fieldByKey(k.Value); f != nil {
			l.setNode(f, v)
			continue
		}
		if !contains(sections, k.Value) {
			l.unknown(k, k.Value, topKeys)
			continue
		}
		if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
			continue
		}
		if v.Kind != yaml.MappingNode {
			l.fileErr(v, k.Value, "expected a mapping of settings")
			continue
		}
		var children []string
		for _, f := range Fields {
			if sec, name, ok := strings.Cut(f.Key, "."); ok && sec == k.Value {
				children = append(children, name)
			}
		}
		seenChild := map[string]bool{}
		for j := 0; j+1 < len(v.Content); j += 2 {
			ck, cv := v.Content[j], v.Content[j+1]
			key := k.Value + "." + ck.Value
			if seenChild[ck.Value] {
				l.fileErr(ck, key, "duplicate key")
				continue
			}
			seenChild[ck.Value] = true
			f := fieldByKey(key)
			if f == nil {
				l.unknown(ck, key, children)
				continue
			}
			l.setNode(f, cv)
		}
	}
}

func (l *loader) unknown(n *yaml.Node, field string, candidates []string) {
	name := field
	if _, last, ok := strings.Cut(field, "."); ok {
		name = last
	}
	msg := "unknown setting"
	if s := suggest(name, candidates); s != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	} else {
		msg += fmt.Sprintf(" (known: %s)", strings.Join(candidates, ", "))
	}
	l.fileErr(n, field, "%s", msg)
}

func (l *loader) setNode(f *Field, n *yaml.Node) {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return // `key:` with no value: leave unset
	}
	switch f.Kind {
	case kMap:
		if n.Kind != yaml.MappingNode {
			l.fileErr(n, f.Key, "expected a mapping")
			return
		}
		m := map[string]any{}
		if err := n.Decode(&m); err != nil {
			l.fileErr(n, f.Key, "%v", err)
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if err := CheckSuiteName(n.Content[i].Value); err != nil {
				l.fileErr(n.Content[i], f.Key, "%v", err)
				return
			}
		}
		l.cfg.Suites = m
		l.set(f, SourceFile, false)
	case kSection:
		if n.Kind != yaml.MappingNode {
			l.fileErr(n, f.Key, "expected a mapping")
			return
		}
		m := map[string]any{}
		if err := n.Decode(&m); err != nil {
			l.fileErr(n, f.Key, "%v", err)
			return
		}
		l.cfg.Explore = m
		l.set(f, SourceFile, false)
	case kRepos:
		l.setRepos(f, n)
	case kList:
		if n.Kind != yaml.SequenceNode {
			l.fileErr(n, f.Key, "expected a list, e.g. [failed, passed]")
			return
		}
		var items []string
		for _, it := range n.Content {
			if it.Kind != yaml.ScalarNode {
				l.fileErr(it, f.Key, "expected a plain value")
				return
			}
			items = append(items, it.Value)
		}
		auto, err := applyString(&l.cfg, f, strings.Join(items, ","))
		if err != nil {
			l.fileErr(n, f.Key, "%v", err)
			return
		}
		l.set(f, SourceFile, auto)
	default:
		if n.Kind != yaml.ScalarNode {
			l.fileErr(n, f.Key, "expected a single value")
			return
		}
		if err := checkTag(f, n); err != nil {
			l.fileErr(n, f.Key, "%v", err)
			return
		}
		auto, err := applyString(&l.cfg, f, n.Value)
		if err != nil {
			l.fileErr(n, f.Key, "%v", err)
			return
		}
		l.set(f, SourceFile, auto)
	}
}

// checkTag rejects YAML values of the wrong type that the string parser would accept.
func checkTag(f *Field, n *yaml.Node) error {
	switch f.Kind {
	case kBool:
		if n.Tag != "!!bool" {
			return fmt.Errorf("%q is not true or false", n.Value)
		}
	case kInt:
		if n.Tag != "!!int" && !(f.Auto && n.Value == "auto") {
			return fmt.Errorf("%q is not a whole number", n.Value)
		}
	}
	return nil
}

func (l *loader) setRepos(f *Field, n *yaml.Node) {
	if n.Kind != yaml.SequenceNode {
		l.fileErr(n, f.Key, "expected a list of repositories")
		return
	}
	var repos []Repo
	seen := map[string]bool{}
	ok := true
	for i, item := range n.Content {
		path := fmt.Sprintf("repos[%d]", i)
		if item.Kind != yaml.MappingNode {
			l.fileErr(item, path, "expected a mapping with a name, e.g. {name: acme/api}")
			ok = false
			continue
		}
		r := Repo{Checks: true}
		hasName := false
		for j := 0; j+1 < len(item.Content); j += 2 {
			k, v := item.Content[j], item.Content[j+1]
			switch k.Value {
			case "name":
				hasName = true
				switch {
				case v.Kind != yaml.ScalarNode || !repoName.MatchString(v.Value):
					l.fileErr(v, path+".name", "%q is not a repository (expected owner/name)", v.Value)
					ok = false
				case seen[v.Value]:
					l.fileErr(v, path+".name", "%s is listed twice", v.Value)
					ok = false
				default:
					seen[v.Value] = true
					r.Name = v.Value
				}
			case "workflows":
				skip, good := l.workflowSkip(v, path+".workflows")
				ok = ok && good
				r.SkipWorkflows = skip
			case "checks":
				if v.Kind != yaml.ScalarNode || (v.Value != "true" && v.Value != "false") {
					l.fileErr(v, path+".checks", "expected true or false")
					ok = false
					break
				}
				r.Checks = v.Value == "true"
			default:
				l.unknown(k, path+"."+k.Value, []string{"name", "workflows", "checks"})
				ok = false
			}
		}
		if !hasName {
			l.fileErr(item, path, "missing name")
			ok = false
		}
		repos = append(repos, r)
	}
	if ok {
		l.cfg.Repos = repos
		l.set(f, SourceFile, false)
	}
}

func (l *loader) workflowSkip(n *yaml.Node, path string) ([]string, bool) {
	if n.Kind != yaml.MappingNode {
		l.fileErr(n, path, "expected a mapping, e.g. {skip: [release.yml]}")
		return nil, false
	}
	var skip []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value != "skip" {
			l.unknown(k, path+"."+k.Value, []string{"skip"})
			return nil, false
		}
		if v.Kind != yaml.SequenceNode {
			l.fileErr(v, path+".skip", "expected a list of workflow file names")
			return nil, false
		}
		for _, it := range v.Content {
			if it.Kind != yaml.ScalarNode {
				l.fileErr(it, path+".skip", "expected a workflow file name")
				return nil, false
			}
			skip = append(skip, it.Value)
		}
	}
	return skip, true
}

var suiteName = regexp.MustCompile(`^[a-z0-9-]+$`)

// CheckSuiteName says whether name can name a suite. A suite's status contexts are localci/<name> and
// localci/<name> (affected), so the name holds no slash or space, and checks is the workflow checks' summary.
func CheckSuiteName(name string) error {
	switch {
	case name == "checks":
		return fmt.Errorf("the suite name %q is reserved (its status, localci/checks, belongs to workflow checks); pick another name", name)
	case !suiteName.MatchString(name):
		return fmt.Errorf("the suite name %q must use only lowercase letters, digits and dashes", name)
	}
	return nil
}

package workflow

import (
	"fmt"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultTimeout is GitHub's job timeout when timeout-minutes is not set.
const defaultTimeout = 360 * time.Minute

// toolchains maps the setup-* actions localci replaces to the tool name and
// the `with` keys that carry the version.
var toolchains = map[string]struct{ tool, key, file string }{
	"actions/setup-node":   {"node", "node-version", "node-version-file"},
	"actions/setup-java":   {"java", "java-version", "java-version-file"},
	"actions/setup-python": {"python", "python-version", "python-version-file"},
	"actions/setup-go":     {"go", "go-version", "go-version-file"},
}

// planWorkflow plans one workflow file for ev.
func planWorkflow(repo, file string, data []byte, ev Event, sha string) ([]Check, []Skip) {
	wfName := file
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		reason := "the file is not a workflow mapping"
		if err != nil {
			reason = err.Error()
		}
		return []Check{finish(Check{Repo: repo, Workflow: wfName, Job: "-", Name: wfName,
			Unsupported: "invalid workflow file: " + reason})}, nil
	}
	keys, top := mapPairs(root.Content[0])
	var onNode *yaml.Node
	for _, k := range keys {
		// YAML 1.1 readers turn an unquoted `on` into the boolean true.
		if k == "on" || k == "true" || k == "True" {
			onNode = top[k]
		}
	}
	if n := top["name"]; n != nil && scalar(n) != "" {
		wfName = scalar(n)
	}

	trig, ok := readTriggers(onNode)[ev.Name]
	if !ok {
		return nil, []Skip{{Workflow: wfName, Reason: "does not trigger on " + ev.Name}}
	}
	if r := trig.skipReason(ev); r != "" {
		return nil, []Skip{{Workflow: wfName, Reason: r}}
	}

	base := &scope{event: ev, sha: sha, env: map[string]string{}}
	wfBad := trig.invalidPatterns()
	wfEnv := resolveEnv(top["env"], base, &wfBad)
	wfShell, wfDir := runDefaults(top["defaults"])

	var checks []Check
	var skips []Skip
	jobKeys, jobs := mapPairs(top["jobs"])
	for _, id := range jobKeys {
		c, s := planJob(repo, wfName, id, jobs[id], base, wfEnv, wfShell, wfDir, wfBad)
		checks = append(checks, c...)
		skips = append(skips, s...)
	}
	return checks, skips
}

func resolveEnv(n *yaml.Node, sc *scope, bad *[]string) map[string]string {
	keys, vals := mapPairs(n)
	out := map[string]string{}
	for _, k := range keys {
		v, b := sc.subst(scalar(vals[k]))
		*bad = append(*bad, b...)
		out[k] = v
	}
	return out
}

func runDefaults(n *yaml.Node) (shell, dir string) {
	_, d := mapPairs(n)
	_, run := mapPairs(d["run"])
	return scalar(run["shell"]), scalar(run["working-directory"])
}

func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func planJob(repo, wf, id string, n *yaml.Node, base *scope, wfEnv map[string]string, wfShell, wfDir string, wfBad []string) ([]Check, []Skip) {
	_, j := mapPairs(n)

	if u := scalar(j["uses"]); u != "" {
		return []Check{finish(Check{Repo: repo, Workflow: wf, Job: id, Name: id, NeedsActions: true, Timeout: defaultTimeout,
			Unsupported: "reusable workflow " + u + " is not supported"})}, nil
	}

	_, strat := mapPairs(j["strategy"])
	combos, mxBad := expandMatrix(strat["matrix"])
	if j["strategy"] != nil && j["strategy"].Kind == yaml.ScalarNode {
		mxBad = "strategy is computed at run time"
	}

	var checks []Check
	var skips []Skip
	if len(combos) == 0 {
		return nil, []Skip{{Workflow: wf, Job: id, Reason: "strategy.matrix expands to no jobs"}}
	}
	for _, cb := range combos {
		sc := &scope{event: base.event, sha: base.sha, matrix: cb.vals, typed: cb.typed, env: wfEnv}
		bad := append([]string{}, wfBad...)
		if mxBad != "" {
			bad = append(bad, mxBad)
		}
		sub := func(s string) string {
			out, b := sc.subst(s)
			bad = append(bad, b...)
			return out
		}

		rawName := scalar(j["name"])
		name := id
		if rawName != "" {
			name = sub(rawName)
		}
		if !strings.Contains(rawName, "matrix.") {
			name += cb.suffix()
		}

		if cond := scalar(j["if"]); cond != "" {
			run, _, err := sc.condition(cond)
			if err != nil {
				bad = append(bad, "if: "+err.Error())
			} else if !run {
				skips = append(skips, Skip{Workflow: wf, Job: name, Reason: "if: " + cond + " is false"})
				continue
			}
		}

		jobEnv := resolveEnv(j["env"], sc, &bad)
		env := merge(wfEnv, jobEnv)
		sc.env = env
		shell, dir := wfShell, wfDir
		if s, d := runDefaults(j["defaults"]); s != "" || d != "" {
			if s != "" {
				shell = s
			}
			if d != "" {
				dir = d
			}
		}

		c := Check{Repo: repo, Workflow: wf, Job: id, Name: name, Env: nonEmpty(env), Matrix: nonEmpty(cb.vals), Timeout: defaultTimeout}
		if t := scalar(j["timeout-minutes"]); t != "" {
			var m float64
			if _, err := fmt.Sscanf(sub(t), "%g", &m); err == nil && m > 0 {
				c.Timeout = time.Duration(m * float64(time.Minute))
			} else {
				bad = append(bad, "timeout-minutes: "+t)
			}
		}
		c.Container = readContainer(j["container"], sub)
		c.Services = readServices(j["services"], sub)

		if stepNodes := j["steps"]; stepNodes != nil {
			for _, sn := range stepNodes.Content {
				planStep(&c, sn, sc, shell, dir, &bad)
			}
		}
		for _, s := range c.Steps {
			if s.Run != "" {
				for _, t := range dockerTags(s.Run) {
					if !contains(c.DockerTags, t) {
						c.DockerTags = append(c.DockerTags, t)
					}
				}
			}
		}
		c.Unsupported = strings.Join(dedupe(bad), "; ")
		checks = append(checks, finish(c))
	}
	return checks, skips
}

func planStep(c *Check, n *yaml.Node, sc *scope, shell, dir string, bad *[]string) {
	_, s := mapPairs(n)
	sub := func(v string) string {
		out, b := sc.subst(v)
		*bad = append(*bad, b...)
		return out
	}
	st := Step{ID: scalar(s["id"]), Name: sub(scalar(s["name"]))}
	if cond := scalar(s["if"]); cond != "" {
		run, always, err := sc.condition(cond)
		if err != nil {
			*bad = append(*bad, "if: "+err.Error())
		} else if !run {
			return
		} else {
			st.Always = always
		}
	}
	st.ContinueOnError = scalar(s["continue-on-error"]) == "true"
	// Expressions in `with` only matter when the step is kept: a dropped
	// checkout or cache step never reports them.
	withKeys, withNodes := mapPairs(s["with"])
	with := map[string]string{}
	var withBad []string
	for _, k := range withKeys {
		v, b := sc.subst(scalar(withNodes[k]))
		with[k] = v
		withBad = append(withBad, b...)
	}

	if uses := scalar(s["uses"]); uses != "" {
		action, _, _ := strings.Cut(uses, "@")
		if tc, ok := toolchains[action]; ok {
			*bad = append(*bad, withBad...)
			switch {
			case with[tc.key] != "":
				if c.Toolchain == nil {
					c.Toolchain = map[string]string{}
				}
				c.Toolchain[tc.tool] = with[tc.key]
			case with[tc.file] != "":
				if c.Toolchain == nil {
					c.Toolchain = map[string]string{}
				}
				c.Toolchain[tc.tool] = "file:" + with[tc.file]
			}
			return
		}
		// The checkout is done by localci; caches are warm on this machine.
		if action == "actions/checkout" && with["repository"] == "" && with["path"] == "" && with["ref"] == "" {
			return
		}
		if action == "actions/cache" || strings.HasPrefix(action, "actions/cache/") {
			return
		}
		*bad = append(*bad, withBad...)
		st.Uses, st.With = uses, nonEmpty(with)
		if st.Name == "" {
			st.Name = "Run " + uses
		}
		c.NeedsActions = true
	} else {
		st.Run = sub(scalar(s["run"]))
		if st.Run == "" {
			*bad = append(*bad, "step without run or uses")
		}
		if st.Name == "" {
			first, _, _ := strings.Cut(strings.TrimSpace(st.Run), "\n")
			st.Name = "Run " + first
		}
		st.Shell, st.WorkingDirectory = shell, dir
		if v := scalar(s["shell"]); v != "" {
			st.Shell = v
		}
		if v := scalar(s["working-directory"]); v != "" {
			st.WorkingDirectory = sub(v)
		}
	}
	st.Env = nonEmpty(resolveEnv(s["env"], sc, bad))
	c.Steps = append(c.Steps, st)
}

func readContainer(n *yaml.Node, sub func(string) string) *Container {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return &Container{Image: sub(n.Value)}
	}
	_, m := mapPairs(n)
	return &Container{Image: sub(scalar(m["image"])), Env: subMap(m["env"], sub), Ports: subList(m["ports"], sub),
		Volumes: subList(m["volumes"], sub), Options: sub(scalar(m["options"]))}
}

func readServices(n *yaml.Node, sub func(string) string) []Service {
	keys, vals := mapPairs(n)
	var out []Service
	for _, k := range keys {
		_, m := mapPairs(vals[k])
		image := scalar(vals[k])
		if vals[k].Kind == yaml.MappingNode {
			image = scalar(m["image"])
		}
		out = append(out, Service{Name: k, Image: sub(image), Env: subMap(m["env"], sub), Ports: subList(m["ports"], sub),
			Volumes: subList(m["volumes"], sub), Options: sub(scalar(m["options"]))})
	}
	return out
}

func subMap(n *yaml.Node, sub func(string) string) map[string]string {
	keys, vals := mapPairs(n)
	out := map[string]string{}
	for _, k := range keys {
		out[k] = sub(scalar(vals[k]))
	}
	return nonEmpty(out)
}

func subList(n *yaml.Node, sub func(string) string) []string {
	var out []string
	for _, v := range strList(n) {
		out = append(out, sub(v))
	}
	return out
}

func nonEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func dedupe(l []string) []string {
	var out []string
	for _, s := range l {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func finish(c Check) Check {
	c.Key = computeKey(c)
	return c
}

// workflowFile reports whether p is a workflow definition path.
func workflowFile(p string) bool {
	if path.Dir(p) != ".github/workflows" {
		return false
	}
	e := path.Ext(p)
	return e == ".yml" || e == ".yaml"
}

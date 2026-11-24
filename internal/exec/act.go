package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrActMissing is returned by RunAct when `act` is not on PATH.
var ErrActMissing = errors.New("`act` is not installed, and this job uses third-party actions that only it can run: " +
	"install it with `brew install act` (macOS) or see https://nektosact.com/installation.html, make sure Docker is running, then run the check again")

// ActSpec describes one job to run through `act`.
type ActSpec struct {
	Dir string // workspace, at the tree under test
	// Workflow is the workflow's display name (its `name:`) or its path
	// relative to Dir; Job is the job id.
	Workflow string
	Job      string
	// Matrix pins the matrix cell, so only that cell runs.
	Matrix map[string]string
	// Repo is owner/name; PR, Base (branch name), Head (commit) and Merge
	// (the tree under test) fill the pull_request event.
	Repo, Base, Head, Merge string
	PR                      int
	// Run is the id of the run this job belongs to: its containers carry it as a label, so they are removed with
	// the run, or swept at the next start when the run died with them.
	Run     string
	LogPath string
	Timeout time.Duration
}

// ErrActRanNothing is returned, with act's result, when act exited 0 but never
// reported a job as succeeded: a matrix cell that matched nothing, or a job
// act skipped.
var ErrActRanNothing = errors.New("act ran no job")

// actLabelKey tags the containers one act run starts, so a cancelled or timed
// out run can remove exactly them. act 0.2.x has no container name prefix.
const actLabelKey = "localci.act"

// RunLabelKey tags every container a run starts with the run's id (`localci.run=<id>`).
const RunLabelKey = "localci.run"

// removeLabeled removes the containers carrying label, and the service
// containers and network act made for each; a variable so tests need no Docker.
var removeLabeled = RemoveLabeled

// actMatrixValue renders a matrix value the way act will compare it: act
// parses the value as YAML and matches it with the parsed workflow, so the
// float 3.10 is only found as 3.1.
func actMatrixValue(v string) string {
	var x any
	if yaml.Unmarshal([]byte(v), &x) == nil {
		if f, ok := x.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	return v
}

// RunAct runs `act pull_request -j <job> -W <workflow>` in spec.Dir with an
// event payload holding the PR number and refs, logging to spec.LogPath.
func RunAct(ctx context.Context, spec ActSpec) (Result, error) {
	if _, err := osexec.LookPath("act"); err != nil {
		return Result{}, ErrActMissing
	}
	wf, err := ResolveWorkflow(spec.Dir, spec.Workflow, spec.Job)
	if err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(actEvent(spec))
	if err != nil {
		return Result{}, err
	}
	// The payload lives beside the log, never in the workspace: it must not dirty the tree.
	eventPath := filepath.Join(filepath.Dir(spec.LogPath), "act-event.json")
	if err := os.MkdirAll(filepath.Dir(eventPath), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(eventPath, payload, 0o644); err != nil {
		return Result{}, err
	}
	// act runs from a directory of ours, never from the workspace, and every option that has a file default is
	// passed explicitly: a PR that commits an .actrc (--privileged, -v /:/host), .secrets, .env, .vars or .input
	// must not change how act runs it. act reads the first from its own working directory and the others from -C.
	cwd := filepath.Join(filepath.Dir(spec.LogPath), "act-cwd")
	empty := filepath.Join(cwd, "empty")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		return Result{}, err
	}
	workspace, err := filepath.Abs(spec.Dir)
	if err != nil {
		return Result{}, err
	}
	token := make([]byte, 6)
	rand.Read(token)
	label := actLabelKey + "=" + hex.EncodeToString(token)
	labels := "--label " + label
	if spec.Run != "" {
		labels += " --label " + RunLabelKey + "=" + spec.Run
	}
	// act applies --container-options only to a job without `container:`; one with it takes the options written in
	// its own container block and drops ours, so it would carry no label and be found by neither sweep. Those
	// jobs get the labels in a copy of the workflow that lives beside the log (never in the workspace).
	wfPath, err := labelledWorkflow(workspace, wf, filepath.Join(filepath.Dir(spec.LogPath), "act-workflow"), labels)
	if err != nil {
		return Result{}, err
	}
	args := []string{"act", "pull_request", "-j", spec.Job, "-W", wfPath, "-e", eventPath, "-C", workspace,
		"--secret-file", empty, "--env-file", empty, "--var-file", empty, "--input-file", empty}
	args = append(args, actImageArgs()...)
	keys := make([]string, 0, len(spec.Matrix))
	for k := range spec.Matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--matrix", k+":"+actMatrixValue(spec.Matrix[k]))
	}
	args = append(args, "--rm", "--container-options", labels)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuote(a)
	}
	res, err := Run(ctx, Spec{Cmd: strings.Join(quoted, " "), Dir: cwd, LogPath: spec.LogPath, Timeout: spec.Timeout})
	if err != nil {
		return res, err
	}
	if res.TimedOut || res.Cancelled || ctx.Err() != nil {
		// act was killed mid-run and leaves its containers behind.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		removeLabeled(cctx, label)
	}
	if res.OK() && !actReportedSuccess(spec.LogPath) {
		return res, fmt.Errorf("%w for %s", ErrActRanNothing, actTarget(spec))
	}
	return res, nil
}

// labelledWorkflow returns the workflow file act should run: wf itself (relative to workspace) when no job has a
// `container:`, else a copy in dir, same file name, where every such job's container options carry labels.
func labelledWorkflow(workspace, wf, dir, labels string) (string, error) {
	src, err := os.ReadFile(filepath.Join(workspace, filepath.FromSlash(wf)))
	if err != nil {
		return "", err
	}
	out, changed, err := addContainerLabels(src, labels)
	if err != nil || !changed {
		return wf, nil // a workflow act can read but we cannot: act reports it itself, and the job is not swept
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, filepath.Base(wf))
	return p, os.WriteFile(p, out, 0o644)
}

// addContainerLabels appends labels to the `options` of every job's `container:` in the workflow src, turning the
// `container: image` shorthand into the mapping form. changed is false when no job has a container.
func addContainerLabels(src []byte, labels string) (out []byte, changed bool, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, false, err
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, false, nil
	}
	for i := 1; i < len(jobs.Content); i += 2 {
		job := resolveAlias(jobs.Content[i])
		if job.Kind != yaml.MappingNode {
			continue
		}
		for k := 0; k+1 < len(job.Content); k += 2 {
			if job.Content[k].Value != "container" {
				continue
			}
			c := resolveAlias(job.Content[k+1])
			switch c.Kind {
			case yaml.ScalarNode:
				job.Content[k+1] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Tag: "!!str", Value: "image"}, c,
					{Kind: yaml.ScalarNode, Tag: "!!str", Value: "options"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: labels},
				}}
				changed = true
			case yaml.MappingNode:
				if o := mapValue(c, "options"); o != nil && o.Kind == yaml.ScalarNode {
					if !strings.Contains(o.Value, labels) { // an anchored options string is shared by several jobs
						o.Value = strings.TrimSpace(o.Value + " " + labels)
						o.Style = 0
					}
				} else if o == nil {
					c.Content = append(c.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "options"},
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: labels})
				} else {
					continue
				}
				changed = true
			}
		}
	}
	if !changed {
		return nil, false, nil
	}
	b, err := yaml.Marshal(&doc)
	return b, err == nil, err
}

func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// mapValue is the value stored under key in the mapping node m, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	m = resolveAlias(m)
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return resolveAlias(m.Content[i+1])
		}
	}
	return nil
}

// actReportedSuccess reports whether act's log says a job succeeded.
func actReportedSuccess(log string) bool {
	b, err := os.ReadFile(log)
	return err == nil && strings.Contains(string(b), "Job succeeded")
}

func actTarget(spec ActSpec) string {
	keys := make([]string, 0, len(spec.Matrix))
	for k := range spec.Matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{"job " + spec.Job}
	for _, k := range keys {
		parts = append(parts, "matrix "+k+":"+actMatrixValue(spec.Matrix[k]))
	}
	return strings.Join(parts, ", ")
}

// actEvent is the pull_request event payload GitHub would send.
func actEvent(s ActSpec) map[string]any {
	head := s.Head
	if head == "" {
		head = s.Merge
	}
	return map[string]any{
		"action": "synchronize",
		"number": s.PR,
		"pull_request": map[string]any{
			"number":           s.PR,
			"head":             map[string]any{"sha": head, "ref": head},
			"base":             map[string]any{"ref": s.Base},
			"merge_commit_sha": s.Merge,
		},
		"repository": map[string]any{"full_name": s.Repo},
	}
}

// ResolveWorkflow finds the workflow file (relative to dir) that holds job.
// workflow is a path relative to dir or the workflow's `name:`, which is what
// the planner reports.
func ResolveWorkflow(dir, workflow, job string) (string, error) {
	var matches []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(dir, ".github", "workflows", pat))
		matches = append(matches, m...)
	}
	sort.Strings(matches)
	for _, p := range matches {
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == filepath.ToSlash(workflow) {
			return rel, nil
		}
	}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc struct {
			Name string               `yaml:"name"`
			Jobs map[string]yaml.Node `yaml:"jobs"`
		}
		if yaml.Unmarshal(b, &doc) != nil || doc.Name != workflow {
			continue
		}
		if _, ok := doc.Jobs[job]; ok {
			rel, _ := filepath.Rel(dir, p)
			return filepath.ToSlash(rel), nil
		}
	}
	return "", fmt.Errorf("no workflow file named %q with job %q under .github/workflows", workflow, job)
}

// actImage is act's "medium" image, the one it offers by default.
const actImage = "catthehacker/ubuntu:act-latest"

// actImageArgs maps the Ubuntu runner labels to a default image when the user
// has no act configuration. On a first run without one, act asks which image
// to use and dies on the unanswered prompt, which no check could survive. Only
// the user's own configuration counts: a file in the workspace is the PR's.
func actImageArgs() []string {
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".actrc"))
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(cfg, "act", "actrc"))
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		candidates = append(candidates, filepath.Join(x, "act", "actrc"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return nil
		}
	}
	var args []string
	for _, label := range []string{"ubuntu-latest", "ubuntu-24.04", "ubuntu-22.04"} {
		args = append(args, "-P", label+"="+actImage)
	}
	return args
}

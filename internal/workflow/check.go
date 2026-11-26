// Package workflow reads GitHub Actions workflow files and plans the checks a
// pull request would run, with everything needed to run them locally.
package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Event describes the GitHub event the checks are planned for.
type Event struct {
	// Name is the event name: "pull_request" or "push".
	Name string
	// Repo is the repository as owner/name. Empty means: derive it from the
	// checkout's origin remote, else from the directory name.
	Repo string
	// Branch is the branch the event targets: the PR's base branch, or the
	// pushed branch. Empty disables branch filters.
	Branch string
	// BaseRef is the git ref the change is compared with (merge-base diff).
	// It is only used when Files is nil.
	BaseRef string
	// Files are the changed paths. nil with no BaseRef means "unknown": path
	// filters are then not applied and the check is planned.
	Files []string
	// PR is the pull request number and HeadBranch its head branch, for
	// github.ref and github.head_ref. Zero and empty mean unknown.
	PR         int
	HeadBranch string
}

// Check is one job (one matrix cell) a workflow would run.
type Check struct {
	Repo     string            `json:"repo"`
	Workflow string            `json:"workflow"`
	Job      string            `json:"job"`
	Name     string            `json:"name"`
	Matrix   map[string]string `json:"matrix,omitempty"`
	Steps    []Step            `json:"steps,omitempty"`
	// Container is the job container, run steps execute inside it.
	Container *Container `json:"container,omitempty"`
	Services  []Service  `json:"services,omitempty"`
	// Toolchain maps a tool (node, java, python, go) to the version a setup-*
	// step asked for. A "file:<path>" value names a version file in the repo.
	Toolchain map[string]string `json:"toolchain,omitempty"`
	// Env is the workflow and job environment, job values win.
	Env map[string]string `json:"env,omitempty"`
	// Timeout is timeout-minutes, GitHub's default (6 h) when not set.
	Timeout    time.Duration `json:"-"`
	DockerTags []string      `json:"dockerTags,omitempty"`
	// NeedsActions is set when a step uses an action localci cannot replace
	// with a native command.
	NeedsActions bool `json:"needsActions,omitempty"`
	// Unsupported, when set, says why the plan may differ from what GitHub
	// would run. The check is still planned.
	Unsupported string `json:"unsupported,omitempty"`
	// Key is a hash of the normalized definition: same definition, same key.
	Key string `json:"key"`
}

// Step is one step of a job.
type Step struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Run is the shell script; Uses is the action reference. Exactly one is set.
	Run  string            `json:"run,omitempty"`
	Uses string            `json:"uses,omitempty"`
	With map[string]string `json:"with,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// Shell and WorkingDirectory come from the step or from defaults.run.
	Shell            string `json:"shell,omitempty"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// Always means the step also runs after an earlier step failed
	// (if: always()).
	Always          bool `json:"always,omitempty"`
	ContinueOnError bool `json:"continueOnError,omitempty"`
}

// Container is a job container.
type Container struct {
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	Ports   []string          `json:"ports,omitempty"`
	Volumes []string          `json:"volumes,omitempty"`
	Options string            `json:"options,omitempty"`
}

// Service is a service container of a job.
type Service struct {
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Env     map[string]string `json:"env,omitempty"`
	Ports   []string          `json:"ports,omitempty"`
	Volumes []string          `json:"volumes,omitempty"`
	Options string            `json:"options,omitempty"`
}

// Skip says why a workflow, or one of its jobs, is not planned for the event.
type Skip struct {
	Workflow string `json:"workflow"`
	// Job is empty when the whole workflow is skipped.
	Job    string `json:"job,omitempty"`
	Reason string `json:"reason"`
}

// Result is the full outcome of planning: what runs and what does not.
type Result struct {
	Checks  []Check `json:"checks"`
	Skipped []Skip  `json:"skipped"`
}

// MarshalJSON renders the timeout as a duration string ("10m0s").
func (c Check) MarshalJSON() ([]byte, error) {
	type plain Check
	return json.Marshal(struct {
		plain
		Timeout string `json:"timeout"`
	}{plain(c), c.Timeout.String()})
}

// computeKey hashes the definition of c: everything but the repository
// (which tree it runs on is not part of the definition) and the key itself.
func computeKey(c Check) string {
	c.Repo, c.Key = "", ""
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

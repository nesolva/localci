// Package forge talks to code hosts: it watches pull requests for new heads and
// labels, resolves the heads a multi-repo suite should test, and posts commit
// statuses. GitHub is the only implementation; Forge is the seam for others.
package forge

import (
	"context"
	"errors"
)

// TriggerKind says why a run should start.
type TriggerKind string

const (
	// KindPush is a new head on an open pull request.
	KindPush TriggerKind = "push"
	// KindLabel is a trigger label added to an open pull request.
	KindLabel TriggerKind = "label"
)

// Trigger is one reason to run: a pull request got a new head or a label.
type Trigger struct {
	Kind   TriggerKind `json:"kind"`
	Repo   string      `json:"repo"`   // owner/name of the repository the PR targets
	PR     int         `json:"pr"`     // pull request number
	Branch string      `json:"branch"` // head branch name
	Head   string      `json:"head"`   // head commit sha
	Base   string      `json:"base"`   // base branch name
	Author string      `json:"author"`
	Label  string      `json:"label,omitempty"` // set for KindLabel
	// Fork is set when the head lives in another repository: its branch name
	// says nothing about the branches of the other repos.
	Fork bool `json:"fork,omitempty"`
	// Attempt counts the emissions of a push trigger for this head: 1 the
	// first time, more when the handler answered ErrNotReady before.
	// LastAttempt is set on the last one the watcher makes.
	Attempt     int  `json:"attempt,omitempty"`
	LastAttempt bool `json:"lastAttempt,omitempty"`
}

// ErrNotReady, wrapped in the error an emit callback returns for a push
// trigger, says GitHub has not caught up with the head yet (its pull request
// refs are behind): the trigger is not consumed and the next poll emits the
// same head again, at most MaxAttempts times in all.
var ErrNotReady = errors.New("not ready yet")

// MaxAttempts is how many polls in a row a head is emitted while its handler
// answers ErrNotReady.
const MaxAttempts = 10

// Head is the commit of one repository a run tests.
type Head struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	PR     int    `json:"pr,omitempty"` // 0 when the repo uses its default branch
}

// State is a commit status state.
type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"
)

// Status is a commit status posted as `localci/<Suite>`.
type Status struct {
	Suite       string
	State       State
	Description string // at most 140 characters
	TargetURL   string
}

// Forge is a code host.
type Forge interface {
	// Watch blocks, calling emit once per new head and once per label added,
	// until ctx is done. A push trigger whose emit returns ErrNotReady is
	// emitted again by the next poll, up to MaxAttempts times.
	Watch(ctx context.Context, emit func(Trigger) error) error
	// ResolveHeads returns, for each repo, the head to test for trigger t: the
	// trigger's own head for its repo, else the open PR with the same branch
	// name (never for a fork trigger), else the default branch.
	ResolveHeads(ctx context.Context, repos []string, t Trigger) ([]Head, error)
	// SetStatus posts a commit status.
	SetStatus(ctx context.Context, repo, sha string, st Status) error
}

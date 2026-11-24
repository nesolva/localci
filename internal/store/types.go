package store

import "encoding/json"

// Run kinds.
const (
	KindSuite = "suite"
	KindCheck = "check"
)

// Run states, in lifecycle order.
const (
	StateQueued    = "queued"
	StatePreparing = "preparing"
	StateRunning   = "running"
	StatePassed    = "passed"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateReused    = "reused"
)

// Terminal reports whether a run in this state will not change again on its own.
func Terminal(state string) bool {
	switch state {
	case StatePassed, StateFailed, StateCancelled, StateReused:
		return true
	}
	return false
}

// Times are unix milliseconds; 0 means not yet. Shards number from 1; shard 0 means "not tied to a shard".

// Run is a suite over repo heads, or a check.
type Run struct {
	ID          string          `json:"id"`
	Suite       string          `json:"suite"`
	Kind        string          `json:"kind"`
	Heads       json.RawMessage `json:"heads,omitempty"`
	Trigger     string          `json:"trigger,omitempty"`
	RequestedBy string          `json:"requested_by,omitempty"`
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"`
	Created     int64           `json:"created"`
	Started     int64           `json:"started,omitempty"`
	Finished    int64           `json:"finished,omitempty"`
	// Fork says the run tests a pull request head that lives in another repository: its report is not trusted
	// to teach the coverage map.
	Fork bool `json:"fork,omitempty"`
	// Affected says the run was asked for only the tests its diff can affect; its status is localci/<suite>
	// (affected), never the suite's own.
	Affected bool `json:"affected,omitempty"`
	// Infra is why the stack or the tenant flow broke, as one printable line (see report.Reason); "" when it did
	// not. A run with it is an infrastructure failure, not failed tests: its interrupted tests are not counted as
	// failed and nothing is learned from it.
	Infra string `json:"infra,omitempty"`
}

// Shard is one slice of a run. CostCPU is cores, CostMem bytes (measured, 0 until known).
type Shard struct {
	Run     string  `json:"run"`
	N       int     `json:"n"`
	State   string  `json:"state"`
	Worker  string  `json:"worker,omitempty"`
	LogPath string  `json:"log_path,omitempty"`
	CostCPU float64 `json:"cost_cpu,omitempty"`
	CostMem int64   `json:"cost_mem,omitempty"`
}

// Test is one attempt (Retry) of a test in a run. ID is the stable test id.
type Test struct {
	Run      string          `json:"run"`
	Shard    int             `json:"shard"`
	ID       string          `json:"id"`
	File     string          `json:"file"`
	Title    string          `json:"title"`
	Line     int             `json:"line,omitempty"`
	Status   string          `json:"status"`
	MS       int64           `json:"ms"`
	Retry    int             `json:"retry"`
	Failure  json.RawMessage `json:"failure,omitempty"`
	Class    string          `json:"class,omitempty"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

// Step is one step of a test. OK is nil while the step is still running.
type Step struct {
	Run    string `json:"run"`
	Test   string `json:"test"`
	ID     string `json:"step"`
	Parent string `json:"parent,omitempty"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Start  int64  `json:"start"`
	MS     int64  `json:"ms"`
	OK     *bool  `json:"ok,omitempty"`
}

// Artifact is a file a test produced (screenshot, trace, video, aria snapshot).
type Artifact struct {
	Run  string `json:"run"`
	Test string `json:"test,omitempty"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// Event is one row of the event log. Seq is assigned by the store, global and strictly increasing.
type Event struct {
	Seq   int64           `json:"seq"`
	TS    int64           `json:"ts"`
	Run   string          `json:"run,omitempty"`
	Shard int             `json:"shard,omitempty"`
	Kind  string          `json:"kind"`
	Test  string          `json:"test,omitempty"`
	Step  string          `json:"step,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// SerialEntry is a test that must run alone, with the evidence that put it there.
type SerialEntry struct {
	Suite    string          `json:"suite"`
	Test     string          `json:"test"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
	Since    int64           `json:"since"`
}

// Lane is the steps of one shard in start order.
type Lane struct {
	Shard int            `json:"shard"`
	Steps []TimelineStep `json:"steps"`
}

// TimelineStep is a step placed on the timeline: Offset is ms from the run's first activity.
type TimelineStep struct {
	Test   string `json:"test"`
	ID     string `json:"step"`
	Parent string `json:"parent,omitempty"`
	Title  string `json:"title"`
	Kind   string `json:"kind"`
	Offset int64  `json:"offset"`
	MS     int64  `json:"ms"`
	OK     *bool  `json:"ok,omitempty"`
}

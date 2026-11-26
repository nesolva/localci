package server

// The JSON the local API speaks. The agent CLI decodes the same types, so a field added here reaches both.

// Head is one repository a run tests.
type Head struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha,omitempty"`
	PR     int    `json:"pr,omitempty"`
	// Dir is the checkout whose working tree the run tests; Dirty says it held uncommitted tracked changes.
	Dir   string `json:"dir,omitempty"`
	Dirty bool   `json:"dirty,omitempty"`
}

// Run is a suite run or a check, as the lists and the run page show it.
type Run struct {
	ID     string `json:"id"`
	Suite  string `json:"suite"`
	Kind   string `json:"kind"` // suite | check
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Infra is why the stack or the tenant flow broke (one printable line); set, the run is an infrastructure
	// failure: its status is error, `wait` exits 3 and its interrupted tests are not counted as failed.
	Infra string `json:"infra,omitempty"`
	// Outcome says why a failed run failed when the exit code depends on it: `timeout` (the suite's timeout stopped
	// it, `wait` exits 4) or `no_tests_matched` (an explicit filter selected no test, exit 2).
	Outcome  string `json:"outcome,omitempty"`
	Trigger  string `json:"trigger,omitempty"`
	Heads    []Head `json:"heads,omitempty"`
	Created  int64  `json:"created"`
	Started  int64  `json:"started,omitempty"`
	Finished int64  `json:"finished,omitempty"`
	// Tests is how many tests the run plans (or has recorded, when more); 0 for a check.
	Tests  int `json:"tests"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Shards int `json:"shards,omitempty"`
	// Affected: the run chose only the tests its diff can affect; it posts localci/<suite> (affected).
	Affected bool `json:"affected,omitempty"`
	// ETAMS is the time still needed, from earlier runs' medians; 0 when unknown.
	ETAMS int64 `json:"eta_ms,omitempty"`
}

// ShardLabel is the label of shard n: "serial" for 0, the lane of the serial group and the reruns.
func ShardLabel(n int) string {
	if n == 0 {
		return "serial"
	}
	return ""
}

// ShardInfo is one shard of a run with where its log lives.
type ShardInfo struct {
	N     int    `json:"n"`
	State string `json:"state"`
	// Label names the shard when its number does not: "serial" for shard 0 (the serial group and the reruns),
	// empty for the others.
	Label   string `json:"label"`
	LogPath string `json:"log_path,omitempty"`
}

// RunDetail is a run with its shards.
type RunDetail struct {
	Run
	ShardList []ShardInfo `json:"shard_list,omitempty"`
}

// RunList answers GET /api/runs. Cursor is the newest event number at the time of the call: pass it back as
// ?since= to get only the runs touched since.
type RunList struct {
	Runs   []Run `json:"runs"`
	Total  int   `json:"total"`
	Cursor int64 `json:"cursor"`
}

// NetworkError is a failed request seen by the page.
type NetworkError struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status"`
}

// Failure is the agent-ready report of one failed test.
type Failure struct {
	Test       string         `json:"test"`
	Title      string         `json:"title"`
	File       string         `json:"file"`
	Line       int            `json:"line,omitempty"`
	Shard      int            `json:"shard"`
	Label      string         `json:"label"` // the shard's ShardInfo.Label
	MS         int64          `json:"ms"`
	Status     string         `json:"status"`
	Class      string         `json:"class,omitempty"`
	Step       string         `json:"step,omitempty"`
	Message    string         `json:"message,omitempty"`
	Expected   string         `json:"expected,omitempty"`
	Actual     string         `json:"actual,omitempty"`
	Location   string         `json:"location,omitempty"`
	Aria       string         `json:"aria,omitempty"`
	Console    []string       `json:"console,omitempty"`
	Network    []NetworkError `json:"network,omitempty"`
	Screenshot string         `json:"screenshot,omitempty"`
	Trace      string         `json:"trace,omitempty"`
}

// FailureList answers GET /api/runs/{id}/failures. Total counts every failure of the run, Failures only the ones
// newer than ?since=.
type FailureList struct {
	Run      Run       `json:"run"`
	Total    int       `json:"total"`
	Failures []Failure `json:"failures"`
	Cursor   int64     `json:"cursor"`
}

// SubmitRequest is the body of POST /api/runs.
type SubmitRequest struct {
	Suite  string   `json:"suite,omitempty"`
	Checks bool     `json:"checks,omitempty"` // run the pull request checks of the checkout in Dir
	Grep   string   `json:"grep,omitempty"`
	Failed string   `json:"failed,omitempty"` // a run id: only the tests that failed in it
	Files  []string `json:"files,omitempty"`
	// Dir is the checkout the command ran in; its working tree is what gets tested.
	Dir   string `json:"dir,omitempty"`
	Force bool   `json:"force,omitempty"` // checks: run even when the same tree already passed
	// Affected runs only the tests the diff can affect; Base is the branch it starts from (default branch when
	// empty) and Explain stops after the choice, running no test.
	Affected bool   `json:"affected,omitempty"`
	Base     string `json:"base,omitempty"`
	Explain  bool   `json:"explain,omitempty"`
}

// Skipped is a check that was left out, and why.
type Skipped struct {
	Workflow string `json:"workflow"`
	Job      string `json:"job"`
	Reason   string `json:"reason"`
}

// SubmitResult answers POST /api/runs.
type SubmitResult struct {
	Runs    []Run     `json:"runs"`
	Skipped []Skipped `json:"skipped,omitempty"`
	Cursor  int64     `json:"cursor"`
}

// Queue answers GET /api/queue: what is running or waiting, oldest first.
type Queue struct {
	Runs    []Run `json:"runs"`
	Waiting int   `json:"waiting"` // admissions waiting for machine capacity
}

// Worker is a machine that runs shards: this one first, then every worker that joined.
type Worker struct {
	Name       string `json:"name"`
	CPUs       int    `json:"cpus"`
	UsedCPUs   int    `json:"used_cpus"`
	Memory     uint64 `json:"memory"`
	UsedMemory uint64 `json:"used_memory"`
	Running    int    `json:"running"`
	// Remote workers (joined with a token): how many shards they take at once, whether they are connected, which
	// shards they run, and when they were last heard from (unix ms).
	Remote   bool     `json:"remote,omitempty"`
	Capacity int      `json:"capacity,omitempty"`
	Status   string   `json:"status,omitempty"` // online | offline | revoked
	Shards   []string `json:"shards,omitempty"`
	LastSeen int64    `json:"last_seen,omitempty"`
}

// Workers answers GET /api/workers.
type Workers struct {
	Workers []Worker `json:"workers"`
}

// Error is the body of every failed call.
type Error struct {
	Error string `json:"error"`
}

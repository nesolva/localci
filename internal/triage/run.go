package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/nesolva/localci/internal/store"
)

// Evidence is what triage stores in tests.evidence next to what the runner put there (a note about the serial
// group, say).
type Evidence struct {
	Lines []string   `json:"lines"`
	Note  string     `json:"note,omitempty"`
	Patch *PatchInfo `json:"patch,omitempty"`
}

// EvidenceJSON merges the verdict into the evidence already stored for the test.
func EvidenceJSON(existing json.RawMessage, e Evidence) json.RawMessage {
	m := map[string]any{}
	if len(existing) > 0 {
		json.Unmarshal(existing, &m)
	}
	m["lines"] = e.Lines
	delete(m, "note")
	if e.Note != "" {
		m["note"] = e.Note
	}
	delete(m, "patch")
	if e.Patch != nil {
		m["patch"] = e.Patch
	}
	out, _ := json.Marshal(m)
	return out
}

// ReadEvidence reads what a test's evidence holds; evidence written by someone else's code (the serial group's
// note) reads as a line of its own.
func ReadEvidence(raw json.RawMessage) Evidence {
	var e Evidence
	if len(raw) == 0 {
		return e
	}
	json.Unmarshal(raw, &e)
	if len(e.Lines) == 0 {
		var other struct {
			Note string `json:"note"`
		}
		if json.Unmarshal(raw, &other) == nil && other.Note != "" {
			e.Lines = []string{other.Note}
		}
	}
	return e
}

// Workspace says where the test files of a run were checked out and which commits a patch to them fits.
type Workspace struct {
	Dir   string   // the test repository's workspace
	Files []string // the suite's test files, as listed
	Shas  []string // the tested commit and, for a working tree, its base
}

// RunInput is everything Run needs to triage a finished run.
type RunInput struct {
	Run       string
	Tests     []string             // ids in the order they should be judged
	Attempts  map[string][]Attempt // by test id
	Steps     map[string]int       // steps recorded per test id
	Moved     map[string]bool      // tests the runner moved to the serial group
	Health    Health
	Changed   []string
	Workspace Workspace
	LogDir    string // patches go under <LogDir>/triage
}

// Result counts what Run did.
type Result struct {
	Classified int
	Flaky      int
}

// Run judges every test of the run that failed at some point and stores class and evidence on its last attempt.
func Run(ctx context.Context, st *store.Store, in RunInput) (Result, error) {
	var res Result
	for _, id := range in.Tests {
		attempts := in.Attempts[id]
		steps, known := in.Steps[id]
		if !known {
			steps = -1
		}
		v, ok := Classify(Input{Attempts: attempts, Steps: steps, Health: in.Health, Changed: in.Changed})
		if !ok {
			continue
		}
		// A test the runner moved to the serial group fails only next to others: that is its explanation.
		if in.Moved[id] && v.Class != Infrastructure {
			continue
		}
		ev := Evidence{Lines: v.Lines, Note: v.Note}
		if v.Fix != nil {
			p, why := proposeFix(in, id, attempts, *v.Fix)
			ev.Patch = p
			switch {
			case p != nil:
				ev.Lines = append(ev.Lines, fmt.Sprintf("fix: replace %s with %s in %s (localci triage %s --apply)",
					quote(clip(v.Fix.From, 60)), quote(clip(v.Fix.To, 60)), p.File, in.Run))
			case why != "":
				ev.Lines = append(ev.Lines, why)
			}
		}
		if len(ev.Lines) > 3 {
			ev.Lines = ev.Lines[:3]
		}
		var old json.RawMessage
		if t, err := st.RunTest(ctx, in.Run, id); err == nil {
			old = t.Evidence
		}
		if err := st.SetClass(ctx, in.Run, id, v.Class, EvidenceJSON(old, ev)); err != nil {
			return res, err
		}
		res.Classified++
		if v.Class == Flaky {
			res.Flaky++
		}
	}
	return res, nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// proposeFix writes the patch of a test bug under the log directory.
func proposeFix(in RunInput, id string, attempts []Attempt, fix Fix) (*PatchInfo, string) {
	var loc string
	for i := len(attempts) - 1; i >= 0; i-- {
		if a := attempts[i]; a.Failed() && a.Failure != nil && a.Failure.Location != nil {
			loc = *a.Failure.Location
			break
		}
	}
	if loc == "" || cutText(loc) {
		return nil, "no patch: the failure has no file:line location"
	}
	patch, why := BuildPatch(in.Workspace.Dir, loc, in.Workspace.Files, fix)
	if patch == nil {
		return nil, why
	}
	dir := filepath.Join(in.LogDir, "triage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "no patch: cannot write under the run's log directory"
	}
	path := filepath.Join(dir, fileSafe(id)+".patch")
	if err := os.WriteFile(path, []byte(patch.Diff), 0o644); err != nil {
		return nil, "no patch: cannot write under the run's log directory"
	}
	return &PatchInfo{Path: path, File: patch.File, Shas: in.Workspace.Shas, From: fix.From, To: fix.To}, ""
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// fileSafe is a test id as a file name: the id itself when it is plain (the report wrote it), else a hash.
func fileSafe(id string) string {
	if plainName.MatchString(id) && id != "." && id != ".." {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

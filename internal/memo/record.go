// Package memo plans memoized execution: tests that begin with the same flows share that prefix, which runs once
// and is checkpointed, and every test below it restores from it. The package is pure: it learns prefixes from
// the flow events of a run (Recorder), keeps them between runs (Load, Save) and hands tests to shards in an
// order that builds each shared node once (Planner). It knows nothing about runs, shards' processes or the store.
package memo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const fileName = "memo-prefixes.json"

// sig names one flow call: the flow and its (already redacted) inputs. It is not the checkpoint key, which embeds
// the build and so changes with every commit; a prefix learned on one commit still describes the next one.
func sig(name string, inputs map[string]any) string {
	b, _ := json.Marshal(inputs) // maps marshal with sorted keys
	sum := sha256.Sum256(append([]byte(name+"\x00"), b...))
	return name + ":" + hex.EncodeToString(sum[:])[:12]
}

func join(sigs []string) string { return strings.Join(sigs, "\x00") }

// Recorder learns each test's prefix from the report's events. It is safe for the shards' goroutines.
type Recorder struct {
	mu   sync.Mutex
	per  map[string]*rec
	done map[string][]string // test -> prefix of its last attempt, passed tests only
}

type rec struct {
	prefix []string
	cut    bool           // a real step or a flow without a checkpoint ended the prefix
	stack  []string       // signatures of the flows open now, outermost first
	begun  map[string]int // flow step -> index among the test's top-level flows, -1 for a nested flow
	tops   int
}

func NewRecorder() *Recorder {
	return &Recorder{per: map[string]*rec{}, done: map[string][]string{}}
}

// Begin starts an attempt: the last attempt of a test is the one that counts.
func (r *Recorder) Begin(test string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.per[test] = &rec{begun: map[string]int{}}
}

func (r *Recorder) get(test string) *rec {
	c := r.per[test]
	if c == nil {
		c = &rec{begun: map[string]int{}}
		r.per[test] = c
	}
	return c
}

// Step is a step.begin of the given kind. A test's own step or expectation outside any flow ends its prefix;
// hooks are fixtures' setup, not the test.
func (r *Recorder) Step(test, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.get(test); (kind == "step" || kind == "expect") && len(c.stack) == 0 {
		c.cut = true
	}
}

// FlowBegin is a flow.begin; inputs are the redacted ones from the event.
func (r *Recorder) FlowBegin(test, step, name string, inputs map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(test)
	idx := -1
	if len(c.stack) == 0 {
		idx = c.tops
		c.tops++
	}
	c.begun[step] = idx
	c.stack = append(c.stack, sig(name, inputs))
}

// FlowEnd is a flow.end (checkpoint: saved, restored or none). For a top-level flow it returns its index among
// the test's top-level flows and its signature, which the planner matches against the node it was building.
func (r *Recorder) FlowEnd(test, step, checkpoint string) (index int, signature string, topLevel bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(test)
	if len(c.stack) == 0 {
		return 0, "", false
	}
	signature = c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	index, ok := c.begun[step]
	if !ok || index < 0 {
		return 0, "", false
	}
	if !c.cut {
		if checkpoint == "none" {
			c.cut = true
		} else {
			c.prefix = append(c.prefix, signature)
		}
	}
	return index, signature, true
}

// End closes the attempt; a passed test's prefix is kept.
func (r *Recorder) End(test string, passed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.get(test)
	delete(r.per, test)
	if passed {
		r.done[test] = c.prefix
	} else {
		delete(r.done, test)
	}
}

// Learned is the prefix of every test whose last attempt passed.
func (r *Recorder) Learned() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.done))
	for k, v := range r.done {
		out[k] = append([]string(nil), v...)
	}
	return out
}

type file struct {
	V     int                 `json:"v"`
	Tests map[string][]string `json:"tests"`
}

// Load reads the prefixes learned by earlier runs from dir. A missing file is no prefixes and no error; a file it
// cannot read is an error, and the caller plans nothing and relearns.
func Load(dir string) (map[string][]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, fileName))
	if os.IsNotExist(err) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return map[string][]string{}, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil || f.V != 1 {
		if err == nil {
			err = os.ErrInvalid
		}
		return map[string][]string{}, err
	}
	if f.Tests == nil {
		f.Tests = map[string][]string{}
	}
	return f.Tests, nil
}

// Save merges what a run learned into the file: tests it did not run keep their earlier prefix. The write is
// atomic, so a kill never leaves half a file.
func Save(dir string, learned map[string][]string) error {
	old, err := Load(dir)
	if err != nil {
		old = map[string][]string{} // an unreadable file is replaced
	}
	for k, v := range learned {
		old[k] = v
	}
	b, err := json.Marshal(file{V: 1, Tests: old})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, fileName+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, fileName))
}

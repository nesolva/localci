// Package report reads the report protocol (docs/protocol.md): the NDJSON file a test
// framework package appends to and the core tails.
package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Event is one line of the report file.
//
// Body is a pointer to the typed payload for a known Type (*TestBegin, *StepEnd, …) and nil
// for a type this version does not know: those are kept with their Raw line, never fatal.
type Event struct {
	V    int
	Type string
	TS   int64 // unix milliseconds
	Raw  json.RawMessage
	Body any
}

// Known reports whether the event's type has a typed Body.
func (e Event) Known() bool { return e.Body != nil }

type Begin struct {
	Shard int `json:"shard"`
	Total int `json:"total"`
}

type TestBegin struct {
	Test  string `json:"test"`
	File  string `json:"file"`
	Title string `json:"title"`
	Line  int    `json:"line"`
	Retry int    `json:"retry"`
}

type StepBegin struct {
	Test   string  `json:"test"`
	Step   string  `json:"step"`
	Parent *string `json:"parent"`
	Title  string  `json:"title"`
	Kind   string  `json:"kind"` // step | hook | expect | flow
}

type StepEnd struct {
	Step string  `json:"step"`
	MS   float64 `json:"ms"`
	OK   bool    `json:"ok"`
}

// NetworkError is a failed or erroring request seen during a test.
type NetworkError struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Status int    `json:"status"` // 0: no response
}

// Failure is the agent-ready report of a failed test.
type Failure struct {
	Step       string         `json:"step"`
	Message    string         `json:"message"`
	Expected   *string        `json:"expected"`
	Actual     *string        `json:"actual"`
	Location   *string        `json:"location"`
	Aria       *string        `json:"aria"`
	Console    []string       `json:"console"`
	Network    []NetworkError `json:"network"`
	Screenshot *string        `json:"screenshot"`
	Trace      *string        `json:"trace"`
}

type TestEnd struct {
	Test    string   `json:"test"`
	Status  string   `json:"status"` // passed | failed | timedout | skipped | interrupted
	MS      float64  `json:"ms"`
	Retry   int      `json:"retry"`
	Failure *Failure `json:"failure,omitempty"`
}

type Artifact struct {
	Test string `json:"test"`
	Kind string `json:"kind"` // screenshot | trace | video | aria
	Path string `json:"path"`
}

type FlowBegin struct {
	Test   string         `json:"test"`
	Step   string         `json:"step"`
	Flow   string         `json:"flow"`
	Inputs map[string]any `json:"inputs"`
	Key    string         `json:"key"`
}

type FlowEnd struct {
	Step       string         `json:"step"`
	Outputs    map[string]any `json:"outputs"`
	Checkpoint string         `json:"checkpoint"` // saved | restored | none
}

type Coverage struct {
	Test  string   `json:"test"`
	Files []string `json:"files"`
	// Incomplete says some source the test ran could not be attributed to a file: the test gets no known
	// coverage, so it is always selected.
	Incomplete bool `json:"incomplete,omitempty"`
}

type Writes struct {
	Test   string   `json:"test"`
	Tables []string `json:"tables"`
}

type End struct {
	Status string `json:"status"`
}

// bodies maps each type of protocol v1 to a constructor of its payload.
var bodies = map[string]func() any{
	"begin":      func() any { return new(Begin) },
	"test.begin": func() any { return new(TestBegin) },
	"step.begin": func() any { return new(StepBegin) },
	"step.end":   func() any { return new(StepEnd) },
	"test.end":   func() any { return new(TestEnd) },
	"artifact":   func() any { return new(Artifact) },
	"flow.begin": func() any { return new(FlowBegin) },
	"flow.end":   func() any { return new(FlowEnd) },
	"coverage":   func() any { return new(Coverage) },
	"writes":     func() any { return new(Writes) },
	"end":        func() any { return new(End) },
}

// IsType reports whether t is a type of the report protocol (whatever the version of a given line).
func IsType(t string) bool { return bodies[t] != nil }

// Parse decodes one line. A line that is not a JSON object with a type is an error; an unknown
// type, or a version other than 1, is not: the event comes back with a nil Body.
func Parse(line []byte) (Event, error) {
	line = bytes.TrimSpace(line)
	var head struct {
		V  int    `json:"v"`
		T  string `json:"t"`
		TS int64  `json:"ts"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Event{}, fmt.Errorf("report: %w", err)
	}
	if head.T == "" {
		return Event{}, errors.New("report: event has no type")
	}
	ev := Event{V: head.V, Type: head.T, TS: head.TS, Raw: append(json.RawMessage(nil), line...)}
	if mk := bodies[head.T]; mk != nil && head.V == 1 {
		body := mk()
		if err := json.Unmarshal(line, body); err != nil {
			return Event{}, fmt.Errorf("report: %s: %w", head.T, err)
		}
		ev.Body = body
	}
	return ev, nil
}

// Limits on the free text of a failure, whatever the framework package sent.
const (
	maxConsole = 500
	maxURL     = 500
	maxText    = 2000
	maxAria    = 8000
	maxList    = 20
)

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func clipPtr(p *string, n int) *string {
	if p == nil {
		return nil
	}
	s := clip(*p, n)
	return &s
}

// evidenceURL is a request URL without its userinfo, query string and fragment: those carry tokens.
func evidenceURL(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
		return clip(u.String(), maxURL)
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	return clip(raw, maxURL)
}

// Sanitize bounds what a failure can carry to the dashboard: the network URLs keep origin and path only and every
// free-text field is capped. The framework package does the same; this is the core not trusting it.
func (f *Failure) Sanitize() {
	f.Step = clip(f.Step, maxText)
	f.Message = clip(f.Message, maxText)
	f.Expected, f.Actual = clipPtr(f.Expected, maxText), clipPtr(f.Actual, maxText)
	f.Location = clipPtr(f.Location, maxText)
	f.Aria = clipPtr(f.Aria, maxAria)
	if len(f.Console) > maxList {
		f.Console = f.Console[len(f.Console)-maxList:]
	}
	for i, c := range f.Console {
		f.Console[i] = clip(c, maxConsole)
	}
	if len(f.Network) > maxList {
		f.Network = f.Network[len(f.Network)-maxList:]
	}
	for i := range f.Network {
		f.Network[i].URL = evidenceURL(f.Network[i].URL)
		f.Network[i].Method = clip(f.Network[i].Method, 16)
	}
}

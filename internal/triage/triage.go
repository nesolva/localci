// Package triage says why a test failed. Every failed test (and every test that failed and then passed) gets a
// class and the evidence behind it, so an agent knows whether to fix the app, fix the test or ignore it; a broken
// selector comes with a patch the agent can apply, never a silent "healing".
//
// The failure is untrusted: it is written by the code under test. Classify works on the sanitized report only, and
// a text the sanitizer had to cut is no evidence.
package triage

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nesolva/localci/internal/report"
)

// Classes.
const (
	ProductBug     = "product-bug"
	TestBug        = "test-bug"
	Flaky          = "flaky"
	Infrastructure = "infrastructure"
	Unknown        = "unknown"
)

// Attempt is one execution of a test: its status and, when it did not pass, the failure it reported.
type Attempt struct {
	Status  string // passed | failed | timedOut | interrupted | skipped
	Failure *report.Failure
	// Rerun says the attempt was a rerun alone (the report of a rerun carries no steps).
	Rerun bool
}

// Failed reports whether the attempt did not pass.
func (a Attempt) Failed() bool {
	switch a.Status {
	case "failed", "timedOut", "interrupted":
		return true
	}
	return false
}

// Health is what the run knows about the trustworthiness of its reports.
type Health struct {
	Unreadable int // report lines nobody could read (a `begin` of another version counts as one)
	Invalid    int // events that claimed a type they may not use, or an unknown one
}

// Input is everything Classify looks at for one test.
type Input struct {
	Attempts []Attempt // oldest first
	// Steps is how many steps the first attempt recorded; -1 when unknown.
	Steps   int
	Health  Health
	Changed []string // files the tested tree changed (diff of the head against its parent)
}

// Fix is a proposed replacement for the text a locator looks for.
type Fix struct {
	Method string // getByRole, getByLabel, ...
	Role   string // for getByRole
	From   string
	To     string
}

// Verdict is the class of a test and why.
type Verdict struct {
	Class string
	// Lines is the evidence in at most three short lines.
	Lines []string
	// Note is set when a test that passed in the end still failed on the way: the run stays green, the note stays.
	Note string
	// Fix is set for a test bug whose broken locator has one obvious replacement.
	Fix *Fix
	// Locator is the failing locator as the test wrote it, when the failure named one.
	Locator string
}

// Classify judges a test with at least one failed attempt. ok is false for a test that never failed.
func Classify(in Input) (v Verdict, ok bool) {
	var fails, passes int
	var last *Attempt // the last failed attempt
	passedAfterFail := false
	for i := range in.Attempts {
		a := &in.Attempts[i]
		switch {
		case a.Failed():
			fails++
			last = a
		case a.Status == "passed":
			passes++
			if fails > 0 {
				passedAfterFail = true
			}
		}
	}
	if last == nil {
		return Verdict{}, false
	}
	f := usable(last.Failure)

	if lines := brokenReport(in.Health); lines != nil {
		return Verdict{Class: Infrastructure, Lines: lines}, true
	}
	if lines := infrastructure(in, last, f); lines != nil {
		return withNote(Verdict{Class: Infrastructure, Lines: lines}, in), true
	}
	if passedAfterFail {
		lines := []string{fmt.Sprintf("failed %d, passed %d of %d attempts on the same tree", fails, passes, fails+passes)}
		if m := firstLine(f.Message); m != "" {
			lines = append(lines, "failed with: "+clip(m, 140))
		}
		return withNote(Verdict{Class: Flaky, Lines: lines}, in), true
	}
	if lines := productBug(f); lines != nil {
		return Verdict{Class: ProductBug, Lines: lines}, true
	}
	loc, near := locate(f)
	if near != nil && near.found {
		v := Verdict{Class: TestBug, Lines: near.lines, Locator: loc.Text}
		if near.fix != nil {
			v.Fix = near.fix
		}
		return v, true
	}
	if loc != nil && near != nil && !near.found {
		// The target is gone and nothing on the page looks like it: the product may have dropped it.
		if page := touchedPage(in.Changed, f); page != "" {
			return Verdict{Class: ProductBug, Locator: loc.Text, Lines: []string{
				"target " + clip(loc.Text, 100) + " is gone from the page",
				"the change touches that page: " + page,
			}}, true
		}
	}
	return Verdict{Class: Unknown, Lines: []string{noSignal(f, fails)}}, true
}

func withNote(v Verdict, in Input) Verdict {
	if len(in.Attempts) > 0 && in.Attempts[len(in.Attempts)-1].Status == "passed" {
		v.Note = "failed on the way, passed in the end: the run is green"
	}
	return v
}

// usable is the failure with every text the sanitizer cut taken out: a cut message is no evidence.
func usable(f *report.Failure) report.Failure {
	if f == nil {
		return report.Failure{}
	}
	out := *f
	if cutText(out.Message) {
		out.Message = ""
	}
	if out.Expected != nil && cutText(*out.Expected) {
		out.Expected = nil
	}
	if out.Actual != nil && cutText(*out.Actual) {
		out.Actual = nil
	}
	if out.Aria != nil && cutText(*out.Aria) {
		out.Aria = nil
	}
	out.Console = nil
	for _, c := range f.Console {
		if !cutText(c) {
			out.Console = append(out.Console, c)
		}
	}
	return out
}

// cutText reports whether s ends with the mark Sanitize leaves where it cut a text.
func cutText(s string) bool { return strings.HasSuffix(s, "…") }

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func noSignal(f report.Failure, fails int) string {
	switch {
	case f.Message == "" && f.Expected == nil:
		return "no usable failure text (cut or missing)"
	case fails > 1:
		return fmt.Sprintf("failed %d times, no 5xx, console error or near match", fails)
	}
	return "no 5xx, console error or near match on the page"
}

func brokenReport(h Health) []string {
	if h.Unreadable == 0 && h.Invalid == 0 {
		return nil
	}
	var parts []string
	if h.Unreadable > 0 {
		parts = append(parts, fmt.Sprintf("%d unreadable report lines (or a report of another version)", h.Unreadable))
	}
	if h.Invalid > 0 {
		parts = append(parts, fmt.Sprintf("%d invalid report events", h.Invalid))
	}
	return []string{"the report is broken: " + strings.Join(parts, ", "), "what it says about this test cannot be trusted"}
}

var infraSignals = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`(?i)ERR_CONNECTION_REFUSED|ECONNREFUSED|connection refused`), "connection refused to the stack"},
	{regexp.MustCompile(`(?i)\bOOM\b|out of memory|ENOMEM|exit code 137|signal: killed|\bSIGKILL\b`), "the process ran out of memory or was killed"},
	{regexp.MustCompile(`(?i)no space left on device|ENOSPC|disk full`), "disk full"},
	{regexp.MustCompile(`(?i)target (page, context or browser )?(has been )?(closed|crashed)|page crashed|browser (has )?(crashed|been closed)|browser closed`), "the browser crashed"},
}

func infrastructure(in Input, last *Attempt, f report.Failure) []string {
	var lines []string
	text := f.Message + "\n" + strings.Join(f.Console, "\n")
	for _, n := range f.Network {
		if n.Status == 0 {
			text += "\n" + n.Method + " " + n.URL + " failed"
		}
	}
	for _, s := range infraSignals {
		if m := s.re.FindString(text); m != "" {
			lines = append(lines, s.what+": "+clip(m, 60))
			break
		}
	}
	if last.Status == "interrupted" {
		why := "the stack or the test command ended before the test finished"
		if f.Message != "" {
			why = clip(f.Message, 140)
		}
		lines = append(lines, why)
	}
	if last.Status == "timedOut" && in.Steps == 0 && !in.Attempts[0].Rerun {
		lines = append(lines, "timed out before the first step")
	}
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return lines
}

func productBug(f report.Failure) []string {
	assertion := strings.Contains(f.Message, "expect(") || f.Expected != nil || f.Actual != nil
	if !assertion {
		return nil
	}
	var evidence []string
	for _, n := range f.Network {
		if n.Status >= 500 && n.Status < 600 {
			evidence = append(evidence, fmt.Sprintf("network: %s %s answered %d", n.Method, n.URL, n.Status))
			break
		}
	}
	for _, c := range f.Console {
		// The browser's own note about a failed load is the network entry again; anything else came from the app.
		if !strings.HasPrefix(c, "Failed to load resource") {
			evidence = append(evidence, "console: "+clip(c, 140))
			break
		}
	}
	if len(evidence) == 0 {
		return nil
	}
	lead := "assertion failed"
	switch {
	case f.Expected != nil && f.Actual != nil:
		lead = fmt.Sprintf("assertion failed: expected %s, got %s", clip(*f.Expected, 50), clip(*f.Actual, 50))
	case f.Expected != nil:
		lead = fmt.Sprintf("assertion failed: expected %s", clip(*f.Expected, 70))
	}
	return append([]string{lead}, evidence...)
}

package triage

import (
	"strings"
	"testing"

	"github.com/nesolva/localci/internal/report"
)

func sp(s string) *string { return &s }

func failed(f report.Failure) Attempt { return Attempt{Status: "failed", Failure: &f} }

const renamedAria = `- heading "Checkout" [level=1]
- button "Place order"
- link "Back to cart"`

func TestClassify(t *testing.T) {
	notFound := func(aria string) report.Failure {
		return report.Failure{
			Message: "Test timeout of 30000ms exceeded.\nLocator: getByRole('button', { name: 'Pay' })\nwaiting for getByRole('button', { name: 'Pay' })",
			Aria:    sp(aria),
		}
	}
	tests := []struct {
		name    string
		in      Input
		class   string
		inLines string // substring expected in the joined evidence
	}{
		{
			name:  "renamed button is a test bug with a fix",
			in:    Input{Attempts: []Attempt{failed(notFound(renamedAria))}, Steps: 3},
			class: TestBug, inLines: "Place order",
		},
		{
			name: "assertion with a 5xx is a product bug",
			in: Input{Attempts: []Attempt{failed(report.Failure{
				Message:  "expect(locator).toHaveText(expected)",
				Expected: sp("Thank you"),
				Actual:   sp("Something went wrong"),
				Network:  []report.NetworkError{{Method: "POST", URL: "http://localhost:3000/api/pay", Status: 500}},
			})}, Steps: 4},
			class: ProductBug, inLines: "answered 500",
		},
		{
			name: "assertion with an app console error is a product bug",
			in: Input{Attempts: []Attempt{failed(report.Failure{
				Message: "expect(locator).toBeVisible()",
				Console: []string{"TypeError: cart is undefined"},
			})}, Steps: 2},
			class: ProductBug, inLines: "TypeError",
		},
		{
			name: "failed then passed on the same tree is flaky",
			in: Input{Attempts: []Attempt{
				failed(report.Failure{Message: "expect(locator).toBeVisible()"}),
				{Status: "passed", Rerun: true},
			}, Steps: 2},
			class: Flaky, inLines: "failed 1, passed 1",
		},
		{
			name:  "connection refused is infrastructure",
			in:    Input{Attempts: []Attempt{failed(report.Failure{Message: "page.goto: net::ERR_CONNECTION_REFUSED at http://localhost:3000/"})}, Steps: 0},
			class: Infrastructure, inLines: "connection refused",
		},
		{
			name:  "timeout before the first step is infrastructure",
			in:    Input{Attempts: []Attempt{{Status: "timedOut", Failure: &report.Failure{Message: "Test timeout of 30000ms exceeded."}}}, Steps: 0},
			class: Infrastructure, inLines: "before the first step",
		},
		{
			name:  "interrupted test is infrastructure",
			in:    Input{Attempts: []Attempt{{Status: "interrupted"}}, Steps: 1},
			class: Infrastructure, inLines: "ended before",
		},
		{
			name: "a cut message is no evidence",
			in: Input{Attempts: []Attempt{failed(report.Failure{
				Message: "expect(locator).toHaveText(expected) " + strings.Repeat("x", 50) + "…",
				Network: []report.NetworkError{{Method: "GET", URL: "http://localhost:3000/api/x", Status: 502}},
				Console: nil,
			})}, Steps: 3},
			class: Unknown,
		},
		{
			name: "target gone and the diff touched its page is a product bug",
			in: Input{Attempts: []Attempt{failed(report.Failure{
				Message: "Test timeout of 30000ms exceeded.\nLocator: getByRole('link', { name: 'Invoices' })\nwaiting for getByRole('link', { name: 'Invoices' })\nat http://localhost:3000/billing",
				Aria:    sp(`- heading "Billing" [level=1]`),
			})}, Steps: 2, Changed: []string{"app/billing/page.tsx"}},
			class: ProductBug, inLines: "gone from the page",
		},
		{
			name: "strict mode violation is a test bug without a fix",
			in: Input{Attempts: []Attempt{failed(report.Failure{
				Message: "strict mode violation: getByRole('button', { name: 'Add' }) resolved to 3 elements",
			})}, Steps: 2},
			class: TestBug, inLines: "3 elements",
		},
		{
			name:  "no signal is unknown",
			in:    Input{Attempts: []Attempt{failed(report.Failure{Message: "expect(received).toBe(expected)"})}, Steps: 2},
			class: Unknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := Classify(tc.in)
			if !ok {
				t.Fatal("not classified")
			}
			if v.Class != tc.class {
				t.Fatalf("class = %s (%v), want %s", v.Class, v.Lines, tc.class)
			}
			if len(v.Lines) == 0 || len(v.Lines) > 3 {
				t.Fatalf("evidence has %d lines: %v", len(v.Lines), v.Lines)
			}
			if !strings.Contains(strings.Join(v.Lines, "\n"), tc.inLines) {
				t.Fatalf("evidence %v lacks %q", v.Lines, tc.inLines)
			}
		})
	}
}

func TestClassifyRenameCarriesAFix(t *testing.T) {
	v, _ := Classify(Input{Steps: 3, Attempts: []Attempt{failed(report.Failure{
		Message: "Test timeout of 30000ms exceeded.\nLocator: getByRole('button', { name: 'Pay' })\nwaiting for getByRole('button', { name: 'Pay' })",
		Aria:    sp(renamedAria),
	})}})
	if v.Fix == nil || v.Fix.From != "Pay" || v.Fix.To != "Place order" || v.Fix.Role != "button" {
		t.Fatalf("fix = %+v", v.Fix)
	}
}

func TestClassifyPassedRunKeepsTheFlakyNote(t *testing.T) {
	v, ok := Classify(Input{Steps: 2, Attempts: []Attempt{
		failed(report.Failure{Message: "expect(locator).toBeVisible()"}),
		{Status: "passed"},
	}})
	if !ok || v.Class != Flaky || v.Note == "" {
		t.Fatalf("verdict = %+v ok=%v: a test that passed only on a retry must be flaky with a note", v, ok)
	}
}

func TestClassifyNeverFailedIsNotClassified(t *testing.T) {
	if _, ok := Classify(Input{Attempts: []Attempt{{Status: "passed"}}}); ok {
		t.Fatal("a passing test got a class")
	}
}

func TestClassifyBrokenReportIsInfrastructureWithTheCount(t *testing.T) {
	for name, h := range map[string]Health{
		"unreadable lines (or begin v:2)": {Unreadable: 3},
		"invalid events":                  {Invalid: 2},
	} {
		t.Run(name, func(t *testing.T) {
			// A failure that would be a product bug without the broken report.
			v, _ := Classify(Input{Health: h, Steps: 2, Attempts: []Attempt{failed(report.Failure{
				Message: "expect(locator).toBeVisible()",
				Network: []report.NetworkError{{Method: "GET", URL: "http://localhost/api", Status: 500}},
			})}})
			want := "3 unreadable"
			if h.Invalid > 0 {
				want = "2 invalid"
			}
			if v.Class != Infrastructure || !strings.Contains(v.Lines[0], want) {
				t.Fatalf("verdict = %+v, want infrastructure with %q", v, want)
			}
		})
	}
}

func TestClassifyStrictModeDoesNotNeedAria(t *testing.T) {
	v, _ := Classify(Input{Steps: 1, Attempts: []Attempt{failed(report.Failure{
		Message: "strict mode violation: getByText('Save') resolved to 2 elements",
	})}})
	if v.Class != TestBug || v.Fix != nil {
		t.Fatalf("verdict = %+v", v)
	}
}

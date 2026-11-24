package shard

import (
	"slices"
	"testing"
)

func TestParseObservationAndMeasure(t *testing.T) {
	begin := parseObservation("s\t7\t\t\nc\tpublic.orders\t3\t10\nc\tpublic.plain\t0\t4\n\nDO\n")
	end := parseObservation("s\t9\t\t\nw\t8\tpublic.orders\t\nw\t9\tbilling.invoice\t\nc\tpublic.orders\t5\t12\nc\tpublic.plain\t0\t6\nc\tpublic.idle\t0\t0\n")
	if begin.Seq != 7 || begin.Counters["orders"] != (Counter{3, 10}) {
		t.Fatalf("begin = %+v", begin)
	}
	got := Measure(begin, end)
	if !slices.Equal(got.Writes, []string{"billing.invoice", "orders"}) {
		t.Fatalf("writes = %v", got.Writes)
	}
	if !slices.Equal(got.Reads, []string{"billing.invoice", "orders", "plain"}) {
		t.Fatalf("reads = %v: scanned or written tables, never an idle one", got.Reads)
	}
}

func TestUnionKeepsWhatEarlierRunsLearned(t *testing.T) {
	a := Access{Writes: []string{"x"}, Reads: []string{"x", "y"}}
	b := Access{Writes: []string{}, Reads: []string{"x"}} // this run touched less
	if u := a.Union(b); !slices.Equal(u.Writes, []string{"x"}) || !slices.Equal(u.Reads, []string{"x", "y"}) {
		t.Fatalf("a shrinking set must not forget: %+v", u)
	}
}

func TestSuspectsFlagTheWholeWindowAndConfirmByRerun(t *testing.T) {
	known := map[TestID]Access{"a": {Writes: []string{"orders"}}, "b": {Writes: []string{"audit"}}, "c": {Writes: []string{"mail"}}}
	spans := map[TestID]Span{
		"a": {From: 0, To: 10},
		"b": {From: 5, To: 15},
		"c": {From: 20, Open: true}, // still running when the parallel part ended
	}
	rows := []WriteRow{
		{Seq: 3, Table: "orders"},  // a, explained
		{Seq: 7, Table: "ledger"},  // a and b overlap, nobody knows ledger
		{Seq: 18, Table: "orders"}, // nobody was running: no one is flagged
		{Seq: 25, Table: "mail"},   // c, explained
	}
	got := Suspects(rows, spans, known)
	if len(got) != 2 || got[0].Test != "a" || got[1].Test != "b" {
		t.Fatalf("suspects = %+v, want a and b", got)
	}
	if !slices.Equal(got[0].Unexplained, []string{"ledger"}) || !slices.Equal(got[0].Overlap, []TestID{"b"}) {
		t.Fatalf("evidence = %+v", got[0])
	}
	if !got[0].Confirm([]string{"orders", "ledger"}) {
		t.Fatal("a wrote ledger alone: confirmed")
	}
	if got[1].Confirm([]string{"audit"}) {
		t.Fatal("b alone wrote only what it was known to write: cleared")
	}
}

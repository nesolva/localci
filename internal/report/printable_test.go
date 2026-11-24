package report

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReasonIsOnePrintableLine(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"plain", "POST /api/tenants answered 500", "POST /api/tenants answered 500"},
		{"colour codes leave no parameters behind", "\x1b[31mError:\x1b[0m connect ECONNREFUSED", "Error: connect ECONNREFUSED"},
		{"first line only", "boom\n    at flow (a.ts:1)\nat other", "boom"},
		{"leading blank lines and carriage returns", "\n\r\nboom\r\nmore", "boom"},
		{"title-setting escape", "\x1b]0;owned\x07ok", "ok"},
		{"C0 and C1 controls go, a tab stays", "a\x00b\x07c\x7fd\u0085e\tf", "abcde\tf"},
		{"bidirectional overrides go", "gnp\u202e.exe", "gnp.exe"},
		{"invalid UTF-8", "ok\xff\xfe!", "ok!"},
		{"nothing printable", "\x1b[0m\n\x07", ""},
	} {
		if got := Reason(c.in); got != c.want {
			t.Errorf("%s: Reason(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestReasonIsCappedAtTwoHundredCharacters(t *testing.T) {
	got := Reason(strings.Repeat("é", 500))
	if utf8.RuneCountInString(got) != MaxReason || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %d characters ending %q", utf8.RuneCountInString(got), got[len(got)-3:])
	}
}

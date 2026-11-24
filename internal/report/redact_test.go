package report

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRedactHidesSecretsAtAnyDepthAndCapsStrings(t *testing.T) {
	in := map[string]any{
		"password": "x",
		"user":     "demo",
		"nested":   map[string]any{"apiKey": "k", "ok": "fine", "list": []any{map[string]any{"Secret": "s"}}},
		"long":     strings.Repeat("a", 500),
		"n":        float64(3),
	}
	got := Redact(in)
	b, _ := json.Marshal(got)
	for _, leak := range []string{`"x"`, `"k"`, `"s"`} {
		if strings.Contains(string(b), leak) {
			t.Errorf("%s leaked in %s", leak, b)
		}
	}
	if got["password"] != "***" || got["user"] != "demo" || got["n"] != float64(3) {
		t.Errorf("%v", got)
	}
	if n := len([]rune(got["long"].(string))); n > MaxFlowString+3 {
		t.Errorf("long string kept %d characters", n)
	}
	if in["password"] != "x" {
		t.Error("Redact changed its argument")
	}
}

func TestRedactCoversSessionAuthAndTokenLikeNames(t *testing.T) {
	in := map[string]any{"sessionId": "s1", "Authorization": "Bearer abc", "jwt": "j", "pwd": "p", "userId": "u1"}
	got := Redact(in)
	for _, k := range []string{"sessionId", "Authorization", "jwt", "pwd"} {
		if got[k] != Redacted {
			t.Errorf("%s = %v, want %s", k, got[k], Redacted)
		}
	}
	if got["userId"] != "u1" {
		t.Errorf("userId = %v", got["userId"])
	}
}

// The TypeScript package keeps its own copy of the list; both are tested against this one file.
func TestSensitiveKeysEqualTheSharedProtocolList(t *testing.T) {
	b, err := os.ReadFile("../../protocol/sensitive-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Keys, SensitiveKeys) {
		t.Errorf("SensitiveKeys = %v, protocol/sensitive-keys.json = %v", SensitiveKeys, f.Keys)
	}
}

func TestScrubRemovesURLQueriesCredentialsAndSecretValuesFromFreeText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"GET https://api.acme.test/tenants?token=abc123&x=1 -> 401", "GET https://api.acme.test/tenants -> 401"},
		{"POST https://u:p4ss@acme.test/login#frag failed", "POST https://acme.test/login failed"},
		{"sent Authorization: Bearer eyJhbGciOi.payload.sig", "sent Authorization: ***"},
		{"retried with Bearer eyJhbGciOi.payload.sig", "retried with Bearer ***"},
		{`body {"password": "hunter2", "name": "demo"}`, `body {"password": ***, "name": "demo"}`},
		{"api_key=sk-live-1 and name=demo", "api_key=*** and name=demo"},
		{"POST /api/tenants answered 500", "POST /api/tenants answered 500"},
	}
	for _, c := range cases {
		if got := Scrub(c.in); got != c.want {
			t.Errorf("Scrub(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

package report

import (
	"encoding/json"
	"regexp"
	"strings"
)

// MaxFlowString caps every string of a flow's inputs and outputs.
const MaxFlowString = 200

// Redacted replaces the value of a sensitive key.
const Redacted = "***"

// SensitiveKeys are the case-insensitive substrings of a name that mark its value as a secret. The list is
// protocol/sensitive-keys.json; the TypeScript package keeps the same one and each side tests it equals the file.
var SensitiveKeys = []string{"password", "passwd", "pwd", "secret", "token", "cookie", "key", "session", "auth", "jwt", "bearer", "credential"}

var sensitiveKey = regexp.MustCompile(`(?i)` + strings.Join(SensitiveKeys, "|"))

// Redact returns a copy of v (the decoded JSON of a flow's inputs or outputs) that is safe to store and show:
// whatever sits under a key naming a secret becomes "***" (a nested value too, whole), and strings are capped. The
// framework package does the same before it writes the event; this is the core not trusting it.
func Redact(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	out := make(map[string]any, len(v))
	for k, x := range v {
		if sensitiveKey.MatchString(k) {
			out[k] = Redacted
			continue
		}
		out[k] = redactValue(x)
	}
	return out
}

func redactValue(x any) any {
	switch v := x.(type) {
	case string:
		return clip(v, MaxFlowString)
	case map[string]any:
		return Redact(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = redactValue(e)
		}
		return out
	}
	return x
}

// Sanitize redacts the inputs the event carries.
func (f *FlowBegin) Sanitize() {
	f.Inputs = Redact(f.Inputs)
	f.Flow = clip(f.Flow, 200)
}

// Sanitize redacts the outputs the event carries.
func (f *FlowEnd) Sanitize() { f.Outputs = Redact(f.Outputs) }

// RedactedLine rebuilds the raw line of a flow event from its sanitized body, so what is stored is never what the
// report said. Other fields of the line are kept; the typed ones are replaced.
func RedactedLine(raw json.RawMessage, body any) json.RawMessage {
	var line map[string]any
	if json.Unmarshal(raw, &line) != nil {
		return raw
	}
	b, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	var typed map[string]any
	if json.Unmarshal(b, &typed) != nil {
		return raw
	}
	for k, v := range typed {
		line[k] = v
	}
	out, err := json.Marshal(line)
	if err != nil {
		return raw
	}
	return out
}

var (
	urlInText    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>)\]]+`)
	bearerInText = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)
	secretInText = regexp.MustCompile(`(?i)([A-Za-z0-9_.-]*(?:` + strings.Join(SensitiveKeys, "|") + `)[A-Za-z0-9_.-]*)(["']?\s*[=:]\s*)((?:bearer\s+)?(?:"[^"]*"|'[^']*'|[^\s,;&"']+))`)
)

// Scrub removes what a line of free text written by the code under test can leak: the user, query and fragment of
// every URL, a bearer credential, and the value after a name that marks a secret (`token=abc`, `"password": "x"`).
// Failure texts get the same treatment from Sanitize; use this for text that goes somewhere Sanitize does not reach,
// such as a run's reason and the status posted to the forge.
func Scrub(s string) string {
	s = urlInText.ReplaceAllStringFunc(s, evidenceURL)
	s = secretInText.ReplaceAllString(s, "${1}${2}"+Redacted)
	return bearerInText.ReplaceAllString(s, "Bearer "+Redacted)
}

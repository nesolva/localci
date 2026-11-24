package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigFlagOverridesFileAndShowsSource(t *testing.T) {
	file := writeConfig(t, "repos: [{name: acme/api}]\nwatch:\n  poll: 5m\n")
	var out, errb bytes.Buffer
	if code := run([]string{"config", "--json", "--config", file, "--watch-poll", "30s"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var got struct {
		Watch struct {
			Poll struct{ Value, Source string }
		}
		Repos struct{ Source string }
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.Watch.Poll.Value != "30s" || got.Watch.Poll.Source != "flag" || got.Repos.Source != "file" {
		t.Errorf("got %+v", got)
	}
}

func TestInvalidConfigExitsNonZeroWithLocation(t *testing.T) {
	file := writeConfig(t, "watch:\n  poll: soon\n")
	var out, errb bytes.Buffer
	if code := run([]string{"config", "--config", file}, &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), ":2: watch.poll:") {
		t.Errorf("stderr should name line and field: %s", errb.String())
	}
}

func TestTextOutputIsOneValuePerLineWithSource(t *testing.T) {
	file := writeConfig(t, "repos: [{name: acme/api}]\n")
	var out, errb bytes.Buffer
	run([]string{"config", "--config", file}, &out, &errb)
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n")[1:] {
		if !strings.HasSuffix(line, ")") || !strings.Contains(line, "(") {
			t.Errorf("line without a source: %q", line)
		}
	}
	if !strings.Contains(out.String(), "repos  ") || !strings.Contains(out.String(), "acme/api") {
		t.Errorf("repos missing:\n%s", out.String())
	}
}

func TestConfigShowsTheExploreAgentCommandItDefaultsTo(t *testing.T) {
	file := writeConfig(t, `repos: [{name: acme/shop}]
suites:
  shop: {repos: [acme/shop], test: {repo: acme/shop, run: make e2e}}
explore:
  schedule: "0 2 * * *"
  suite: shop
  base_url: http://127.0.0.1:4310
  goals: [buy something]
  agent: {minutes: 10, env: [AWS_REGION]}
`)
	var out, errb bytes.Buffer
	if code := run([]string{"config", "--config", file}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{`--allowedTools "Bash,Read,Write" --max-turns 60 (default)`, "explore.agent.minutes 10 (file)", "explore.agent.env AWS_REGION (file)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}

package config

import (
	"fmt"
	"strings"
)

//go:generate go run ./gen ../../docs/config.md

// Markdown renders docs/config.md from the schema. `go generate ./...`
// writes it and a test fails when the committed file is stale.
func Markdown() string {
	var b strings.Builder
	b.WriteString("<!-- Generated from internal/config by `go generate ./...`. Do not edit by hand. -->\n\n")
	b.WriteString("# Configuration\n\n")
	b.WriteString("localci reads `~/.config/localci/config.yaml` (`$XDG_CONFIG_HOME/localci/config.yaml` when set). " +
		"Every setting is optional except `repos`. Later layers win: defaults, the file, `LOCALCI_*` environment variables, then flags. " +
		"Run `localci config` to see the effective value of each setting and where it came from " +
		"(`default`, `auto`, `file`, `env` or `flag`).\n\n")
	b.WriteString("Point at another file with `--config path` or `LOCALCI_CONFIG`.\n\n")
	b.WriteString("## Example\n\n```yaml\n")
	b.WriteString(example)
	b.WriteString("```\n\n## Settings\n\n")
	b.WriteString("| Key | Default | Env var | Flag | Description |\n|---|---|---|---|---|\n")
	for _, f := range Fields {
		env, flg := "n/a", "n/a"
		if !f.noEnv {
			env = "`" + EnvName(f.Key) + "`"
			flg = "`--" + FlagName(f.Key) + "`"
		}
		def := f.Default
		if def == "" {
			def = "-"
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s | %s |\n", f.Key, def, env, flg, f.Doc)
	}
	b.WriteString("\n`auto` settings are worked out from the machine when localci starts " +
		"and show the source `auto` in `localci config`. Setting `capacity.cpus` or `capacity.memory` by hand " +
		"also changes the slots that `auto` picks.\n")
	return b.String()
}

const example = `version: 1
repos:
  - name: acme/api
    workflows:
      skip: [release.yml]
  - name: acme/web
    checks: true      # false: suites only, no workflow checks
  - name: acme/local  # lives only on this machine, no origin needed
    path: ~/code/local
    checks: false     # nothing to poll on GitHub
watch:
  poll: 60s
  max_age: 24h
  drafts: false
  forks: label        # never | label | always
capacity:
  cpus: auto
  memory: auto
  docker_memory: auto
  check_slots: auto
  job_slots: auto
fidelity:
  merge_ref: true
  path_filters: true
  toolchains: auto    # mise if installed, else containers
  docker_cache: true
notify:
  on: [failed, passed]
server:
  port: 7357
`

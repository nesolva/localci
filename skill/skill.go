// Package skill carries the agent skill localci ships: it teaches an agent to run the suite, wait, read the
// failures and fix them with `localci` commands.
package skill

import "embed"

// FS holds localci/SKILL.md.
//
//go:embed localci/SKILL.md
var FS embed.FS

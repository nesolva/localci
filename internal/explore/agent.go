package explore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nesolva/localci/internal/exec"
	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/suite"
)

func sortStrings(s []string) { sort.Strings(s) }

// Explorer runs the agent against one shard and stores what it reports.
type Explorer struct {
	Cfg   Config
	Store *store.Store
	Clock Clock
	Flows []string // `localci flows`, for the brief
}

// Run is suite.ExploreRequest.Run: it writes the brief, starts the agent command in the test repository with the
// findings file, kills it when its time is up, and stores the findings. What the agent wrote is kept whichever way
// it ended. It fails only when the agent could not run at all, or exited with an error without reporting anything.
func (x *Explorer) Run(ctx context.Context, env suite.ExploreEnv) (string, error) {
	if x.Clock == nil {
		x.Clock = SystemClock
	}
	if err := os.MkdirAll(env.OutDir, 0o755); err != nil {
		return "", err
	}
	findings := filepath.Join(env.OutDir, "findings.ndjson")
	briefPath := filepath.Join(env.OutDir, "brief.md")
	if err := os.WriteFile(findings, nil, 0o644); err != nil {
		return "", err
	}
	brief := Brief{Goals: x.Cfg.Goals, BaseURL: env.BaseURL, Findings: findings, OutDir: env.OutDir, Minutes: x.Cfg.Agent.Minutes, Flows: x.Flows}
	if err := os.WriteFile(briefPath, []byte(brief.String()), 0o644); err != nil {
		return "", err
	}
	limit := time.Duration(x.Cfg.Agent.Minutes) * time.Minute
	timer := x.Clock.After(limit)

	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	go func() {
		select {
		case <-timer:
			timedOut.Store(true)
			cancel()
		case <-actx.Done():
		}
	}()

	agentEnv := slices.Clone(env.Env)
	for _, name := range x.Cfg.Agent.Env {
		if v, ok := os.LookupEnv(name); ok {
			agentEnv = append(agentEnv, name+"="+v)
		}
	}
	spec := exec.Spec{Cmd: x.Cfg.Agent.Command, Dir: env.Dir, Stdin: briefPath, LogPath: env.LogPath, KillGrace: 5 * time.Second, MinimalEnv: true,
		Env: append(agentEnv, "LOCALCI_FINDINGS="+findings, "LOCALCI_BRIEF="+briefPath,
			"LOCALCI_BASE_URL="+env.BaseURL, "LOCALCI_OUTPUT_DIR="+env.OutDir)}
	env.Emit("explore.start", map[string]any{"goals": len(x.Cfg.Goals), "minutes": x.Cfg.Agent.Minutes})
	p, err := exec.Start(actx, spec)
	if err != nil {
		return "", fmt.Errorf("agent command: %w", err)
	}
	res := p.Wait()

	// Stored even when the run was cancelled: what the agent found is not lost with it.
	sctx := context.WithoutCancel(ctx)
	parsed, err := ReadFindings(findings, env.OutDir, env.Run, env.Suite)
	if err != nil {
		return "", err
	}
	if err := x.Store.AddFindings(sctx, parsed.Findings); err != nil {
		return "", err
	}
	cost, turns, turnLimit := agentCost(env.LogPath)
	done := map[string]any{"findings": len(parsed.Findings), "skipped": parsed.Skipped, "exit": res.ExitCode,
		"timed_out": timedOut.Load(), "turn_limit": turnLimit, "seconds": int(res.Duration.Seconds())}
	switch {
	case timedOut.Load():
		done["ended_by"] = "time limit"
	case turnLimit:
		done["ended_by"] = "turn limit"
	}
	if cost > 0 {
		done["cost_usd"] = cost
		done["turns"] = turns
	}
	env.Emit("explore.done", done)

	note := fmt.Sprintf("%d findings", len(parsed.Findings))
	if len(parsed.Findings) == 1 {
		note = "1 finding"
	}
	if cost > 0 {
		note += fmt.Sprintf(" · $%.2f", cost)
	}
	switch {
	case timedOut.Load():
		note += fmt.Sprintf(" · stopped after %d min", x.Cfg.Agent.Minutes)
	case turnLimit:
		note += fmt.Sprintf(" · stopped at the turn limit (%d turns)", turns)
	case ctx.Err() != nil:
	case res.ExitCode != 0 && len(parsed.Findings) == 0:
		return "", fmt.Errorf("the agent command exited with code %d and reported nothing: %s", res.ExitCode, lastLine(env.LogPath))
	case res.ExitCode != 0:
		note += fmt.Sprintf(" · agent exited with code %d", res.ExitCode)
	}
	return note, nil
}

// agentCost reads what the agent's own result lines report (Claude Code's stream-json writes a result object with
// total_cost_usd and num_turns; a run that resumes or chains sessions writes several, each a running total). It
// returns the largest cost and turn count over all of them, so a short final line cannot hide the long run before it,
// and whether any of them says the agent stopped at its turn limit. Zero values when the output has none.
func agentCost(log string) (usd float64, turns int, turnLimit bool) {
	for _, l := range bytes.Split(tailOf(log, 1<<20), []byte("\n")) {
		l = bytes.TrimSpace(l)
		if !bytes.HasPrefix(l, []byte("{")) || !bytes.Contains(l, []byte(`"result"`)) {
			continue
		}
		var r struct {
			Type    string  `json:"type"`
			Subtype string  `json:"subtype"`
			Cost    float64 `json:"total_cost_usd"`
			Turns   int     `json:"num_turns"`
		}
		if json.Unmarshal(l, &r) != nil || r.Type != "result" {
			continue
		}
		usd = max(usd, r.Cost)
		turns = max(turns, r.Turns)
		turnLimit = turnLimit || r.Subtype == "error_max_turns"
	}
	return usd, turns, turnLimit
}

func tailOf(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(fi.Size()-n, 0)
	buf := make([]byte, fi.Size()-off)
	k, _ := f.ReadAt(buf, off)
	return buf[:k]
}

// lastLine is the last non-empty line of the agent's output, made printable.
func lastLine(path string) string {
	lines := strings.Split(strings.TrimSpace(string(tailOf(path, 8<<10))), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := Text(lines[i], 200); l != "" {
			return l
		}
	}
	return "no output"
}

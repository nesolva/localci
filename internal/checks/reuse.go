package checks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/nesolva/localci/internal/store"
	"github.com/nesolva/localci/internal/workflow"
)

// reuseKey decides whether two runs prove the same thing: the same check
// definition on the same tree with the same resolved tools.
func reuseKey(c workflow.Check, tree, toolchain string) string {
	h := sha256.New()
	for _, part := range []string{c.Repo, c.Key, tree, toolchain} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// findReusable returns the newest passed run of the check with the same reuse key.
func (r *Runner) findReusable(ctx context.Context, name, key string) (store.Run, bool) {
	runs, err := r.Store.Runs(ctx, 0)
	if err != nil {
		return store.Run{}, false
	}
	for _, run := range runs {
		if run.Kind != store.KindCheck || run.State != store.StatePassed || run.Suite != name {
			continue
		}
		if h, ok := parseHead(run.Heads); ok && h.Reuse == key {
			return run, true
		}
	}
	return store.Run{}, false
}

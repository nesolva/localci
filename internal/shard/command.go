package shard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Command is the driver for any engine: the suite config gives shell commands
// with {{template}} and {{name}} placeholders (for Snapshot, {{template}} is
// the database being copied).
type Command struct {
	CreateCmd   string // clone: copy {{template}} to {{name}}
	DropCmd     string // remove {{name}}
	SnapshotCmd string // optional: copy {{template}} to {{name}} while it is live
	// StateDir holds one marker file per built template, so a template is
	// built once per key across runs. Remove the marker to force a rebuild.
	StateDir string
	// Dir is the working directory of the commands.
	Dir string
	Env []string

	once onceMap
}

var _ DB = (*Command)(nil)

func (c *Command) Template(ctx context.Context, key string, seed Seed) (string, error) {
	name := "localci_tpl_" + key
	if err := checkIdent(name); err != nil {
		return "", err
	}
	unlock, err := c.once.lockIn(ctx, c.StateDir, name)
	if err != nil {
		return "", err
	}
	defer unlock()
	marker := filepath.Join(c.StateDir, name)
	if _, err := os.Stat(marker); err == nil {
		return name, nil
	}
	// No marker: whatever exists under this name is the rest of a build that
	// never finished (killed, or failed with nothing to clean up). Start clean.
	c.dropLeftover(ctx, name)
	if err := seed(ctx, name); err != nil {
		c.dropLeftover(ctx, name) // do not keep a half-built template
		return "", fmt.Errorf("seed template %s: %w", name, err)
	}
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return "", err
	}
	return name, os.WriteFile(marker, nil, 0o644)
}

// dropLeftover removes a half-built template. It must work when ctx is already
// cancelled (the likeliest reason a seed failed), so it runs detached with its
// own timeout. A failure is not fatal: the next build tries again.
func (c *Command) dropLeftover(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = c.run(ctx, c.DropCmd, "", name)
}

func (c *Command) Clone(ctx context.Context, template, name string) error {
	return c.run(ctx, c.CreateCmd, template, name)
}

func (c *Command) Drop(ctx context.Context, name string) error {
	if err := checkDroppable(name); err != nil {
		return err
	}
	return c.run(ctx, c.DropCmd, "", name)
}

func (c *Command) Snapshot(ctx context.Context, from, name string) error {
	if c.SnapshotCmd == "" {
		return errors.New("this suite has no snapshot command")
	}
	return c.run(ctx, c.SnapshotCmd, from, name)
}

// Size adds up the files of database name when the commands keep databases as files in Dir (the default of the
// sample suite); a database kept elsewhere is 0 here.
func (c *Command) Size(_ context.Context, name string) (int64, error) {
	if err := checkIdent(name); err != nil {
		return 0, err
	}
	var n int64
	err := filepath.WalkDir(filepath.Join(c.Dir, name), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n, err
}

func (c *Command) run(ctx context.Context, tmpl, template, name string) error {
	if strings.TrimSpace(tmpl) == "" {
		return errors.New("database command is empty")
	}
	for _, v := range []string{template, name} {
		if v != "" {
			if err := checkIdent(v); err != nil { // names reach a shell: keep them plain
				return err
			}
		}
	}
	line := strings.NewReplacer("{{template}}", template, "{{name}}", name).Replace(tmpl)
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("database command %q: %w: %s", line, err, strings.TrimSpace(string(out)))
	}
	return nil
}

package triage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"slices"
	"strings"
)

// PatchInfo is what the evidence of a test bug says about its patch.
type PatchInfo struct {
	Path string   `json:"path"` // the diff, under the run's log directory
	File string   `json:"file"` // the one file it changes, relative to the repository root
	Shas []string `json:"shas"` // the commits whose tree the patch fits: the tested one and, for a working tree, its base
	From string   `json:"from"`
	To   string   `json:"to"`
}

// Refusal is why a patch was not applied; the command prints it as it is.
type Refusal string

func (r Refusal) Error() string { return string(r) }

// Check says whether the patch may be applied to the checkout in dir, and why not when it may not. The run's
// fork flag, the checkout's head and the content of the diff are all looked at: the diff was made from a failure
// written by the code under test, so it only ever changes the one file it names.
func Check(ctx context.Context, dir string, p PatchInfo, fork bool) (diff []byte, root string, err error) {
	if fork {
		return nil, "", Refusal("this run tests a fork's pull request: its patches are never applied automatically")
	}
	diff, err = os.ReadFile(p.Path)
	if err != nil {
		return nil, "", Refusal("the patch file is gone: " + p.Path)
	}
	if err := onlyChanges(string(diff), p.File); err != nil {
		return nil, "", Refusal("the patch is not the one-line change localci wrote: " + err.Error())
	}
	if root, err = gitOut(ctx, dir, nil, "rev-parse", "--show-toplevel"); err != nil {
		return nil, "", Refusal("this directory is not a git checkout")
	}
	if !tracked(ctx, root, p.File) {
		return nil, "", Refusal("the file the patch changes is not a test file tracked by git in this checkout: " + p.File)
	}
	head, err := gitOut(ctx, root, nil, "rev-parse", "HEAD")
	if err != nil {
		return nil, "", Refusal("cannot read this checkout's head")
	}
	if !slices.Contains(p.Shas, head) {
		return nil, "", Refusal(fmt.Sprintf("this checkout is at %s, the run tested %s: check out that commit or run again",
			short(head), strings.Join(shorts(p.Shas), " or ")))
	}
	if _, err := gitOut(ctx, root, diff, "apply", "--check", "-"); err != nil {
		return nil, "", Refusal("the patch does not apply to this working tree: " + err.Error())
	}
	return diff, root, nil
}

// Apply changes the working tree of the checkout in dir and nothing else.
func Apply(ctx context.Context, dir string, p PatchInfo, fork bool) error {
	diff, root, err := Check(ctx, dir, p, fork)
	if err != nil {
		return err
	}
	_, err = gitOut(ctx, root, diff, "apply", "-")
	return err
}

// onlyChanges checks that the diff changes the file named and nothing else: no other file, rename, mode or
// binary change.
func onlyChanges(diff, file string) error {
	files := 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "diff --git "):
			if l != "diff --git a/"+file+" b/"+file {
				return errors.New("it touches another file")
			}
			files++
		case strings.HasPrefix(l, "--- "):
			if l != "--- a/"+file {
				return errors.New("it touches another file")
			}
		case strings.HasPrefix(l, "+++ "):
			if l != "+++ b/"+file {
				return errors.New("it touches another file")
			}
		case strings.HasPrefix(l, "rename "), strings.HasPrefix(l, "copy "), strings.HasPrefix(l, "new file"),
			strings.HasPrefix(l, "deleted file"), strings.HasPrefix(l, "old mode"), strings.HasPrefix(l, "new mode"),
			strings.HasPrefix(l, "GIT binary"), strings.HasPrefix(l, "Binary files"):
			return errors.New("it changes more than file content")
		}
	}
	if files != 1 {
		return errors.New("it does not change exactly one file")
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func shorts(shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = short(s)
	}
	return out
}

// gitOut runs git in dir, feeding stdin when given, and returns its trimmed output.
func gitOut(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
	cmd := osexec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

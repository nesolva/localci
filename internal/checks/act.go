package checks

import (
	"context"
	"errors"
	osexec "os/exec"

	"github.com/nesolva/localci/internal/exec"
)

// dockerUp reports whether Docker answers; a variable so tests need no daemon.
var dockerUp = exec.DockerAvailable

// RunAct is a Runner.Act that runs the job through `act`.
func RunAct(ctx context.Context, j ActJob) (exec.Result, error) {
	if _, err := osexec.LookPath("act"); err != nil {
		return exec.Result{}, exec.ErrActMissing
	}
	// act needs Docker for every job container; without it act dies with an
	// opaque error that would read as a failing check.
	if !dockerUp() {
		return exec.Result{}, errors.New("this check cannot run: Docker is not running, and it runs through act")
	}
	return exec.RunAct(ctx, exec.ActSpec{
		Dir: j.Dir, Workflow: j.Check.Workflow, Job: j.Check.Job, Matrix: j.Check.Matrix,
		Repo: j.Request.Repo, Base: j.Request.Base, Head: j.Request.Head, Merge: j.Merge, PR: j.Request.PR,
		LogPath: j.LogPath, Timeout: j.Check.Timeout, Run: j.Run,
	})
}

package doctor

import "context"

// checkAct looks for `act`, which runs the jobs that use third-party actions.
// Without it those checks fail with the same advice.
func checkAct(ctx context.Context, d Deps, dockerOK bool) Result {
	out, err := d.Run(ctx, "act", "--version")
	if err != nil {
		return Result{"act", Warn, "act not found: jobs that use third-party actions cannot run",
			"install it with `brew install act` (https://nektosact.com/installation.html); it needs Docker"}
	}
	if !dockerOK {
		return Result{"act", Warn, firstLine(out) + " is installed, but Docker is not running: act jobs will report an infrastructure error",
			"start Docker Desktop (or `systemctl start docker`); act runs every job in a container"}
	}
	return Result{"act", OK, firstLine(out), ""}
}

# Troubleshooting

Start with `localci doctor`. It checks what this machine needs (git, a GitHub login, Docker, `mise`, `act`, free disk
and ports, the data directory's permissions) and prints the fix next to each problem.

## "localci is not running"

The CLI talks to a server you start yourself. Run `localci serve` in a terminal, or `localci service install` to keep
it running at login (`localci service status` checks it). The exit code is 3.

## "origin is not an owner/name repository address" or "has no origin remote"

`localci run` finds the repository by the checkout's `origin` remote and matches it with the suite's `repos`. Check
`git remote get-url origin`, and that `owner/name` is in the suite's `repos`.

A checkout with no `origin`, or one whose `origin` does not name the repository, is mapped by hand: give its entry under
`repos:` a `path` (`path: ~/code/shop`, with `checks: false` when it has no pull requests on GitHub). See the
[suites page](suites.md).

## A run ends `infrastructure: …`

The run broke before its tests could say anything: a stack process exited, a service never became ready, the tenant
flow failed, the database template could not be built. Nothing is learned from such a run. `localci logs <id>` gives
the failing lines and the path of the whole log.

## A test fails only in a sharded run

Another test is probably changing the state it reads. localci reruns a test that fails in parallel; one that passes
alone twice goes to the serial group (see [isolation](isolation.md#tests-that-conflict)). `localci triage <id>` names
the tests it overlapped with. To check by hand, `localci run --suite <name> --grep "<title>" --wait`.

## No tests found

The default listing command is `npx --no-install --offline localci-playwright list`. Run it in the repository: it needs
`@nesolva/localci-playwright` installed as a dependency of that repository (the command never downloads it: without the package it stops with an error) and the Playwright config wrapped with `withLocalci`. A suite that uses another
runner sets `test.list` to a command that prints one JSON object per line (`test`, `file`, `title`, `line`).

## Playwright says the browser does not exist

Install one: `npx playwright install chromium`.

## Docker checks fail

Docker is optional for suites, but checks run through `act` and `act` needs it for every job container. When the daemon is
not running, those checks fail at once with "this check cannot run: Docker is not running" instead of running, and
`localci doctor` reports Docker. Start Docker and run the check again.

## The port is taken

The dashboard uses 7357 and the worker listener 7358. Change `server.port` or `server.worker_port` in the config.

## A pull request is not picked up

`localci plan` (from a checkout) lists the checks a pull request would run and why the others are skipped. Also check
`watch.max_age` (older heads only run by hand), `watch.drafts`, and `watch.forks` for a pull request from a fork.

## Which status to require in branch protection

Require `localci/checks`: it is green only when every check of the pull request's head has passed, or none applies. If it
stays pending after a server restart, push again, or retry the checks that show an error: a restart does not repost it.

## Where the files are

`localci config` prints the effective settings and where each came from. State, logs and artifacts are under the data
directory named in `localci doctor` (`$XDG_DATA_HOME/localci` when set).

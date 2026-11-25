# Security model

localci runs code: your tests, your app, your workflows. This page says where that code runs, with whose rights, and
which doors are open. To report a vulnerability see [SECURITY.md](../SECURITY.md).

## What runs where

Everything runs on your machine, as your user, with no sandbox. localci does not send your code, results or
configuration anywhere. It makes these network calls:

- to GitHub, to list pull requests, fetch commits and post commit statuses, with your own credentials;
- whatever your own suites, stacks and workflow steps do;
- to the agent provider, only if you turn on the [exploratory agent](explore.md).

| Component | Runs as | Reachable from |
|---|---|---|
| `localci serve` (runner, watcher, API) | you | this machine only |
| Your stack, tests and workflow steps | you, in a workspace under localci's data directory | ports on 127.0.0.1 |
| Workflow steps that use third-party actions | you, in a container started by `act` | the container's network |
| Dashboard | you | `127.0.0.1:<server.port>`, read-only |
| Worker listener (off by default) | you | the network, only with `server.listen: lan` |
| [Exploratory agent](explore.md) | you, as the agent command you configured | its own shard of the suite |

A pull request's code runs the way a teammate's would on your laptop. Read [forks](#forks) before you watch a public
repository.

Which jobs run follows GitHub. A job is skipped when its `if:` is false for the pull request, or when a job it `needs`
is skipped. A job is also skipped when its `if:` depends on something that is only known while the job runs: a secret,
a variable, or another job's outputs. So a deploy gated that way never runs on your machine. `localci plan` lists each
skipped job and the reason. A workflow you never want run here goes in the repository's `workflows.skip`.

## Credentials

- GitHub: localci uses `$GITHUB_TOKEN`, else the token `gh auth token` prints. It needs access to the repositories you
  list, and permission to write commit statuses.
- The data directory (`~/Library/Application Support/localci` on macOS, `~/.local/share/localci` elsewhere) is private
  to your user. The database and the API token are readable only by you, and `localci doctor` fails when a mode is
  looser than that.

## The local API

The CLI talks to a Unix socket in the data directory. Calls that change anything carry a token that lives in the same
private directory. The TCP port serves the dashboard and read-only calls. It refuses every other call, whatever
token it carries, except the calls a running test makes back to localci, which must carry the token of their own run. A
web page in your browser cannot start or cancel a run. See [the API page](api.md).

## Shards and reports

Each run gets its own token for the calls its shards make back to localci. The report file a shard writes is treated as
untrusted, because the code under test writes it: event types are checked, unreadable lines are counted, and anything
localci plans with (which tests share state, which files a test touches) comes from its own measurements, not from the
report. Failure evidence is trimmed, and values under keys like `password` or `token` in flow inputs and outputs are
replaced before they are stored.

## Forks

`watch.forks` decides what happens with a pull request from a fork:

- `never` runs none.
- `label` (default) runs one only while it has the `ok-to-test` label **and** someone with write access has approved
  its latest commit in a review. Each push needs a new approval, and removing the label stops it. A pull request that
  its author opened from their own fork, when the author has write access, needs no review.
- `always` runs all of them. Use it only on repositories where everyone who can open a pull request is trusted.

The same gate covers every other way a run can start for that pull request, such as triage reruns.

## Workers

Another machine can join to run shards. This is experimental, see [workers](workers.md). The listener is off until you
set `server.listen: lan`. A worker connects to the primary, never the other way; it trusts exactly the certificate
whose fingerprint you gave it, joins with a single-use token and is removed with `localci workers revoke`. Pull requests
from forks stay on the primary unless you set `workers.forks: true`. A worker runs your code too, so only join machines
you would run your tests on anyway.

## The exploratory agent

Off unless you configure `explore`. With the default command it runs Claude Code unattended with the tools `Bash`,
`Read` and `Write`, as you, in the test repository's workspace. It gets a small environment, not your shell's. It is
not sandboxed. [The page about it](explore.md#what-the-default-command-allows) says what it can do and how to allow
less by writing your own `agent.command`.

## Releases

Releases are cut by the maintainer. Each one carries a signed `checksums.txt` and a provenance file; how to check them
is in [CONTRIBUTING.md](../CONTRIBUTING.md#verifying-a-release).

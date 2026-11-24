<!-- Generated from internal/config by `go generate ./...`. Do not edit by hand. -->

# Configuration

localci reads `~/.config/localci/config.yaml` (`$XDG_CONFIG_HOME/localci/config.yaml` when set). Every setting is optional except `repos`. Later layers win: defaults, the file, `LOCALCI_*` environment variables, then flags. Run `localci config` to see the effective value of each setting and where it came from (`default`, `auto`, `file`, `env` or `flag`).

Point at another file with `--config path` or `LOCALCI_CONFIG`.

## Example

```yaml
version: 1
repos:
  - name: acme/api
    workflows:
      skip: [release.yml]
  - name: acme/web
    checks: true      # false: suites only, no workflow checks
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
```

## Settings

| Key | Default | Env var | Flag | Description |
|---|---|---|---|---|
| `version` | `1` | n/a | n/a | Schema version. Only 1 exists. |
| `repos` | `(none)` | `LOCALCI_REPOS` | `--repos` | Repositories to watch, as `owner/name` entries; each may set `workflows.skip` (workflow files to ignore) and `checks: false` (watch it for suites only, run none of its workflow checks). Required to run checks. Env and flag take a comma-separated list of names. |
| `watch.poll` | `60s` | `LOCALCI_WATCH_POLL` | `--watch-poll` | How often to look for new or updated pull requests (at least 10s). |
| `watch.max_age` | `24h` | `LOCALCI_WATCH_MAX_AGE` | `--watch-max-age` | Pull request heads older than this only run when submitted by hand. |
| `watch.drafts` | `false` | `LOCALCI_WATCH_DRAFTS` | `--watch-drafts` | Also run checks on draft pull requests. |
| `watch.forks` | `label` | `LOCALCI_WATCH_FORKS` | `--watch-forks` | Pull requests from forks: `never` runs none, `label` runs one while it has the `ok-to-test` label and someone with write access has approved its latest commit in a review (each new push needs a new approval; removing the label stops it) or its author has write access and the pull request comes from the author's own fork (GitHub does not let authors approve their own pull request; a pull request from someone else's fork always needs the approval). Remaining risk: anyone the author added as a collaborator on their own fork can push to it, and those pushes run without a review. `always` runs all. |
| `capacity.cpus` | `auto` | `LOCALCI_CAPACITY_CPUS` | `--capacity-cpus` | CPUs localci may use. `auto` is the machine's logical CPUs. |
| `capacity.memory` | `auto` | `LOCALCI_CAPACITY_MEMORY` | `--capacity-memory` | Memory localci may use, such as `8GiB`. `auto` is the memory free at startup. |
| `capacity.docker_memory` | `auto` | `LOCALCI_CAPACITY_DOCKER_MEMORY` | `--capacity-docker-memory` | Memory available to Docker, such as `6GiB`. `auto` asks `docker info`; 0 when Docker is not running. |
| `capacity.check_slots` | `auto` | `LOCALCI_CAPACITY_CHECK_SLOTS` | `--capacity-check-slots` | Checks running at once. `auto` follows CPUs, memory and Docker memory. |
| `capacity.job_slots` | `auto` | `LOCALCI_CAPACITY_JOB_SLOTS` | `--capacity-job-slots` | Pull request runs in progress at once. `auto` is half the check slots, at least 1. |
| `fidelity.merge_ref` | `true` | `LOCALCI_FIDELITY_MERGE_REF` | `--fidelity-merge-ref` | Test the pull request's merge result (`refs/pull/N/merge`) instead of its head. |
| `fidelity.path_filters` | `true` | `LOCALCI_FIDELITY_PATH_FILTERS` | `--fidelity-path-filters` | Honour a workflow's `paths` and `paths-ignore`. |
| `fidelity.toolchains` | `auto` | `LOCALCI_FIDELITY_TOOLCHAINS` | `--fidelity-toolchains` | How `setup-*` tool versions are provided. `auto` uses `mise` when installed, else containers. |
| `fidelity.docker_cache` | `true` | `LOCALCI_FIDELITY_DOCKER_CACHE` | `--fidelity-docker-cache` | Give the `docker run` lines of a check a cache volume that survives from one run to the next. |
| `suites` | `{}` | n/a | n/a | E2E suites, each a mapping under its name (lowercase letters, digits and `-`): `repos`, `trigger`, `prepare`, `database`, `stack`, `test`, `shards` (a number or `auto`), `isolation`, `timeout`. See docs/suites.md. |
| `explore` | `(off)` | n/a | n/a | The nightly exploratory agent: `schedule` (cron, five fields), `suite` (the suite whose stack it explores), `base_url` (where the app listens), `goals` (what it tries, in your words), `agent.command`, `agent.minutes` and `agent.env`. Off when the section is absent; when present it needs all of `schedule`, `suite`, `base_url` and at least one goal. See docs/explore.md. |
| `notify.on` | `failed, passed` | `LOCALCI_NOTIFY_ON` | `--notify-on` | Which outcomes of a run you started by hand (`localci run`, a retry) raise a desktop notification: any of `failed`, `passed`. Cancelled runs never do. Env and flag take a comma-separated list. |
| `server.port` | `7357` | `LOCALCI_SERVER_PORT` | `--server-port` | Port of the dashboard (127.0.0.1 only). The CLI uses the API socket in the data directory, not this port. |
| `server.listen` | `local` | `LOCALCI_SERVER_LISTEN` | `--server-listen` | Where the worker listener binds: `local` is 127.0.0.1 only, `lan` is every network interface so another machine can join as a worker. The dashboard and the API socket never leave this machine. |
| `server.worker_port` | `7358` | `LOCALCI_SERVER_WORKER_PORT` | `--server-worker-port` | Port of the worker listener (TLS, worker credentials only). |
| `workers.forks` | `false` | `LOCALCI_WORKERS_FORKS` | `--workers-forks` | Let runs of pull requests from forks use workers. Off by default: a fork's code would run on another machine. |

`auto` settings are worked out from the machine when localci starts and show the source `auto` in `localci config`. Setting `capacity.cpus` or `capacity.memory` by hand also changes the slots that `auto` picks.

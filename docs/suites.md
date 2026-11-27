# Suites

A suite is one e2e run: which repositories it tests, how to prepare them, what to start for every shard, and the test
command. Suites live under `suites:` in `~/.config/localci/config.yaml`. The names are lowercase letters, digits and
`-`; `checks` is reserved. [`examples/`](../examples) has complete ones.

The smallest useful suite needs a repository and a test command:

```yaml
version: 1
repos:
  - name: acme/todo          # the owner/name of the checkout's `origin` remote, or map it with `path`
    checks: false            # no workflow checks, suites only
suites:
  todo:
    repos: [acme/todo]
    test:
      repo: acme/todo
      run: npx playwright test
      env:
        PORT: '{{port "web"}}'   # a port of this shard's own; the Playwright config must read it
    shards: auto
```

A repository that has no `origin` (or whose `origin` does not name it) is mapped to its checkout with `path`:

```yaml
repos:
  - name: acme/todo
    path: ~/code/todo   # the checkout; `localci run` from it tests it as acme/todo
    checks: false       # nothing on GitHub to poll
```

`localci run` finds the repository by the checkout's top-level directory: when it equals a `path`, that entry's `name` is
the repository, whatever `origin` says. Runs clone the repository from that directory instead of GitHub, so keep it for
repositories without pull requests on GitHub, and set `checks: false` (a repository with `path` and `checks: false` is
never polled). `localci doctor` lists each `path` and whether it is a git checkout.

| Key | Meaning |
|---|---|
| `repos` | Repositories the suite tests (required). `localci run` from a checkout tests that working tree, uncommitted tracked changes included; the other repositories use the same branch if it has an open pull request, else their default branch. |
| `trigger` | `push: true` runs it for every new head of a pull request; `label: <name>` when the pull request has that label. |
| `prepare` | Commands run once per run in a warm workspace: `repo`, `run`, `cache_key` (files whose content decides when to run again) and `keep` (paths that survive the next checkout, such as `node_modules`). |
| `database` | Optional. Every shard gets a database cloned from a template. `driver: postgres` (with `container`, `user`) or `driver: command` (with `create`, `drop`, `snapshot`). `template.seed` fills the template and `template.key` lists the files that make it rebuild. |
| `stack` | Optional. Processes started for every shard: `name`, `repo`, `run`, `env`, and `ready` (`http`, `tcp` or `log`, with a `timeout`). |
| `test` | `repo`, `run` (the command for one shard), `env`, and `list` (how to list tests: NDJSON `{test,file,title,line}`; default `npx --no-install --offline localci-playwright list`, for pytest `python -m localci_pytest list`). |
| `shards` | A number, or `auto` (from the machine's capacity). Tests are split by measured duration; a suite with fewer tests than shards leaves some shards empty. |
| `isolation` | `isolated` (default), `shared-db`, `tenant` or `auto`; see [isolation levels](isolation.md). |
| `timeout` | Longest a run may take, such as `10m`; 60 minutes when not set, `0` turns it off. Time spent queued behind other runs does not count. A run it stops ends failed with `timed out after <d>`, and `wait` exits 4. |
| `exclusive` | `true` stops other work while the suite runs. |

## The environment of a run

A run gets the server's environment: that of the `localci serve` process (the service's, when it was installed with
`localci service install`), not the environment of the shell that ran `localci run`. A service starts with what its
manager gives it, which is not your interactive shell's. The result is the same whether you start a run from the CLI,
from a pull request or from the dashboard, and a variable you `export` before `localci run` does not reach the tests.

To give the tests a value, put it in the suite's `env:` under `test` (as in the example above); a `stack` entry has its
own `env` for the processes it starts. Run options will be another way once they ship.

## Templates

Inside `env`, `run`, `ready` and database commands:

| Template | Value |
|---|---|
| `{{port "name"}}` | a free port, the same wherever the same name is used in one shard |
| `{{db.url}}` | this shard's database (a connection URL for Postgres, the file for the `command` driver). In a shell command, quote it (`'{{db.url}}'`): on macOS the file is under `Application Support`, which has a space |
| `{{template}}`, `{{name}}` | in the `command` driver's `create`, `drop` and `snapshot`: the template and the database being made or dropped |

`database.template.seed` and `database.url` belong to the run, not to a shard: `{{port}}` is refused there when the
configuration loads. In the seed, `{{db.url}}` is the template being filled; `database.url` cannot use it.

## In your Playwright project

- Wrap the config: `export default withLocalci(defineConfig({ ... }))` with `withLocalci` from
  `@nesolva/localci-playwright`, and import `test` and `expect` from that package in the specs.
- Read the port from the environment (`process.env.PORT`) in `baseURL` and in `webServer`, and set
  `reuseExistingServer: false` so a shard never talks to someone else's server.
- `@playwright/test` must be a devDependency too (the sample pins `1.64.0`; the package needs 1.49 or newer).

## In your pytest project

For an HTTP API suite, or any pytest suite: `pip install localci-pytest` (Python 3.9 or newer, pytest 7 or newer),
then point `test` at pytest and at the plugin's listing:

```yaml
    test:
      repo: acme/orders
      run: .venv/bin/python -m pytest -p localci_pytest
      list: .venv/bin/python -m localci_pytest list
      env:
        ORDERS_URL: 'http://127.0.0.1:{{port "orders"}}'   # the service under test, started by `stack`
```

- `list` runs without the suite's `env`: read the service URLs when a test runs, not when it is collected.
- Put `pytest.ini` (or `pyproject.toml`) at the repository's root. A test's `file` is relative to pytest's rootdir, and
  file filters and `--affected` read it as relative to the repository.
- [`examples/pytest-api`](../examples/pytest-api) is a complete suite; [the plugin](protocol.md#pytest-plugin) lists what it reports.

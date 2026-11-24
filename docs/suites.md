# Suites

A suite is one e2e run: which repositories it tests, how to prepare them, what to start for every shard, and the test
command. Suites live under `suites:` in `~/.config/localci/config.yaml`. The names are lowercase letters, digits and
`-`; `checks` is reserved. [`examples/`](../examples) has complete ones.

The smallest useful suite needs a repository and a test command:

```yaml
version: 1
repos:
  - name: acme/todo          # must equal the owner/name of the checkout's `origin` remote
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

| Key | Meaning |
|---|---|
| `repos` | Repositories the suite tests (required). `localci run` from a checkout tests that working tree, uncommitted tracked changes included; the other repositories use the same branch if it has an open pull request, else their default branch. |
| `trigger` | `push: true` runs it for every new head of a pull request; `label: <name>` when the pull request has that label. |
| `prepare` | Commands run once per run in a warm workspace: `repo`, `run`, `cache_key` (files whose content decides when to run again) and `keep` (paths that survive the next checkout, such as `node_modules`). |
| `database` | Optional. Every shard gets a database cloned from a template. `driver: postgres` (with `container`, `user`) or `driver: command` (with `create`, `drop`, `snapshot`). `template.seed` fills the template and `template.key` lists the files that make it rebuild. |
| `stack` | Optional. Processes started for every shard: `name`, `repo`, `run`, `env`, and `ready` (`http`, `tcp` or `log`, with a `timeout`). |
| `test` | `repo`, `run` (the command for one shard), `env`, and `list` (how to list tests; default `npx --no-install --offline localci-playwright list`). |
| `shards` | A number, or `auto` (from the machine's capacity). Tests are split by measured duration; a suite with fewer tests than shards leaves some shards empty. |
| `isolation` | `isolated` (default), `shared-db`, `tenant` or `auto`; see [isolation levels](isolation.md). |
| `timeout` | Longest a run may take, such as `10m`. |
| `exclusive` | `true` stops other work while the suite runs. |

## Templates

Inside `env`, `run`, `ready` and database commands:

| Template | Value |
|---|---|
| `{{port "name"}}` | a free port, the same wherever the same name is used in one shard |
| `{{db.url}}` | this shard's database (a connection URL for Postgres, the file for the `command` driver) |
| `{{template}}`, `{{name}}` | in the `command` driver's `create`, `drop` and `snapshot`: the template and the database being made or dropped |

## In your Playwright project

- Wrap the config: `export default withLocalci(defineConfig({ ... }))` with `withLocalci` from
  `@nesolva/localci-playwright`, and import `test` and `expect` from that package in the specs.
- Read the port from the environment (`process.env.PORT`) in `baseURL` and in `webServer`, and set
  `reuseExistingServer: false` so a shard never talks to someone else's server.
- `@playwright/test` must be a devDependency too (the sample pins `1.64.0`; the package needs 1.49 or newer).

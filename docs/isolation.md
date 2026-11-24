# Isolation levels

localci splits a suite into shards and runs them at the same time. The question is what the shards may share. The
suite setting `isolation` picks the answer.

| Level | Each shard gets | Use it when |
|---|---|---|
| `isolated` (default) | its own copy of the stack and its own database, cloned from a template | you want the safe answer, or the stack is cheap to start |
| `shared-db` | its own stack, one database for all | the tables the tests write are disjoint, so shards cannot step on each other |
| `tenant` | one shared stack, and a tenant of its own, made by a flow you name | the app is multi-tenant and starting a stack per shard is the expensive part |
| `auto` | whichever of the above is cheapest and safe for what localci has measured | you would rather not decide |

```yaml
suites:
  e2e:
    isolation: auto
    # for tenant isolation, the flow that creates a tenant and returns what the tests need:
    isolation_tenant_flow: create-tenant
```

## How `auto` decides

localci measures which tables each test writes, using triggers it installs in a schema of its own (named `localci`,
never one of yours) in the database it measures. It records a test's write set only when the test passed, and from
runs where one test at a time touched the database. It picks `shared-db` only when every test's set is known and the
sets of tests in different shards do not overlap. When it cannot tell, it stays on `isolated`.

## Tests that conflict

Some tests touch global state: a setting every user shares, a feature flag, a file. Run in parallel they break each
other. localci never asks you to declare this. A test that fails in parallel and then passes alone twice moves to the
serial group, which runs alone after the parallel part. `localci show` marks such a failure with `class: …` and the
tests it overlapped with, so you can see why.

The serial group, reruns and flake probes run as shard 0.

## Databases

Postgres is handled through its command-line tools (`createdb -T`, `dropdb`). Any other engine works if you give the
suite `create`, `drop` and `snapshot` commands; the sample app, whose database is one JSON file, does this with `cp`.
Every database localci creates starts with `localci_`, and it refuses to drop anything else.

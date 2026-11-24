# Report protocol v1

How a test framework package tells localci what happened in a run. The Playwright package
(`packages/playwright`) speaks it; anything else that can append lines to a file can too. The machine
readable definition is [`protocol/v1.schema.json`](../protocol/v1.schema.json) (JSON Schema, one event
per validation).

## Transport

- localci sets `$LOCALCI_REPORT` to a file path. The package **appends** one JSON object per line
  (NDJSON), each line written with a single `write` so lines from several writers never interleave.
- localci tails the file while the run is going. It may not exist yet; a line without its newline is
  not read until it is complete.
- Every line is `{"v":1,"t":"<type>","ts":<unix ms>, …}`. A reader keeps events of a type it does not
  know and never fails on them, so the protocol can grow without breaking older cores.

localci → package, through the environment:

| Variable | Meaning |
|---|---|
| `LOCALCI_REPORT` | file to append events to |
| `LOCALCI_TESTS` | file of test ids (one per line) this process must run, nothing else |
| `LOCALCI_API` | base URL of the local API; the package calls `POST $LOCALCI_API/hooks/test/begin?test=<id>` before and `…/end?test=<id>` after every test and waits for the answer. A missing or failing endpoint never fails a test |
| `LOCALCI_HOOK_TOKEN` | this run's hook token. Every hook call sends `Authorization: Bearer $LOCALCI_HOOK_TOKEN`; the server refuses a call without it, or with the token of another run. Set it whenever `LOCALCI_API` is set |
| `LOCALCI_SHARD` | 1-based shard index (default 1) |

### Hooks

The package calls the core with `POST $LOCALCI_API/hooks/<area>/<action>?test=<id>`. The areas are `test`
(`begin`, `end`; no body, answered with 204), `flow` and `memo` (below). Every call carries `Authorization: Bearer
$LOCALCI_HOOK_TOKEN`. A hook that is missing, slow or failing never fails a test: the package carries on without the
core's help (a flow just runs).

## Test ids

`id = hash(project, file, title path)`: the project name, the test file relative to the Playwright root
directory (forward slashes) and the describe titles plus the test title. Moving a test within its file
does not change its id, renaming it or its describe block does. The package prints 16 hex characters;
localci treats an id as an opaque string.

## Events

| Type | Fields | Meaning |
|---|---|---|
| `begin` | `shard`, `total` | the process starts; `total` tests will run |
| `test.begin` | `test`, `file`, `title`, `line`, `retry` | an attempt starts; `title` joins describe titles and the test title with ` › ` |
| `step.begin` | `test`, `step`, `parent`, `title`, `kind` | `kind` is `step` (a `test.step`), `hook`, `expect` or `flow`; `parent` is the enclosing step id or `null` |
| `step.end` | `step`, `ms`, `ok` | |
| `artifact` | `test`, `kind`, `path` | `kind` is `screenshot`, `trace`, `video` or `aria`; `path` is absolute. Sent before the matching `test.end` |
| `test.end` | `test`, `status`, `ms`, `retry`, `failure?` | `status`: `passed`, `failed`, `timedout`, `skipped`, `interrupted`; one per attempt |
| `flow.begin` | `test`, `step`, `flow`, `inputs`, `key` | a reusable flow starts; `inputs` are redacted (see below); the `step.begin` of kind `flow` comes first |
| `flow.end` | `step`, `outputs`, `checkpoint`, `ms?`, `reason?` | `checkpoint`: `saved`, `restored`, `none`; `outputs` are redacted; `ms` is how long the call took; `reason` says in a sentence why the flow ran although a checkpoint was possible, or why none was saved |
| `coverage` | `test`, `files`, `incomplete?` | files the test exercised, for affected-test selection. `incomplete: true` says some source it ran could not be attributed to a file: the test then has no known coverage and is always selected |
| `writes` | `test`, `tables` | tables the test wrote; used only when the suite sets `writes: report` (engines the core cannot measure), otherwise stored and ignored: the core measures write sets itself |
| `end` | `status` | the process is done: `passed`, `failed`, `timedout`, `interrupted` |

Step ids are unique inside a run. Only steps a reader can show are sent: `test.step` blocks, hooks and
`expect` calls, not every low-level browser action.

### Redaction

Flow `inputs` and `outputs` are redacted before they reach an event (failure evidence has its own limits, above): every
value under a key that contains one of the names in [`protocol/sensitive-keys.json`](../protocol/sensitive-keys.json) (`password`, `secret`, `token`, `session`, `auth`, `jwt`…; case-insensitive) becomes `***`, at any
depth, and strings are cut to 200 characters. A browser storage state never goes into an event. The core
redacts flow events again when it reads them, because the report file is written by the code under test.

### Events the core adds

Besides the package's events, the core writes its own into the run's event log: `checkpoint.restored`
(`flow`, `key`, `db`), `checkpoint.evicted` (`flow`, `key`, `bytes`) and `db.writes` (tables a test wrote,
reserved). A report cannot forge them: a type of the table above is read as what it is, any other type
starting with `run.`, `shard.`, `step.`, `test.`, `db.` or `checkpoint.` is stored as `report.invalid`.

### `failure`

What an agent needs to act without opening the trace:

| Field | Content |
|---|---|
| `step` | the failing step; the user's `test.step` names down to the failing call, joined with ` › ` |
| `message` | the error message, colour codes removed, at most 2000 characters |
| `expected`, `actual` | the matcher's expected and received values; `null` when the failure is not a matcher failure |
| `location` | absolute `file:line` of the failing call |
| `aria` | accessibility tree of the failing element's nearest landmark (`main`, `form`, `nav`, a dialog…), else of the page body; at most 40 lines, centred on the element |
| `console` | the last 5 console errors and uncaught page errors during the test |
| `network` | the last 5 requests that failed or answered with a status of 400 or more: `{method,url,status}`, `status` 0 when there was no response |
| `screenshot`, `trace` | absolute paths of the files Playwright kept, or `null` |

## Playwright package

```ts
// playwright.config.ts
import { defineConfig } from "@playwright/test";
import { withLocalci } from "@nesolva/localci-playwright";
export default withLocalci(defineConfig({ /* your config */ }));
```

```ts
// in your tests, instead of @playwright/test
import { test, expect } from "@nesolva/localci-playwright";
```

`withLocalci` adds the reporter, keeps a screenshot and a trace for failed tests (unless you set them) and,
when `$LOCALCI_TESTS` is set, runs only those ids. `test` is `@playwright/test`'s `test` with an automatic
fixture that records console errors and failed requests, calls the test hooks on `$LOCALCI_API` and, on
failure, captures the aria excerpt. `npx --no-install --offline localci-playwright list` prints the suite as NDJSON
`{test,file,title,line}`.

## Flows

A flow is a step that several tests start with (sign in, create a tenant), written once with declared inputs
and outputs:

```ts
import { defineFlow } from "@nesolva/localci-playwright";

export const login = defineFlow({
  name: "login",
  inputs: { slug: "string", password: "string" },
  outputs: { user: "string" },
  checkpoint: { browser: true, db: true },
  verify: async ({ page, inputs }) => { /* throws when the restored state is no longer good */ },
  run: async ({ page, inputs }) => { /* the steps */ return { user: "demo" }; },
});
```

Tests call it through the `flow` fixture, by name (`await flow("login", { slug, password })`) or with the
flow itself for typed outputs (`await flow(login, …)`). A flow can call others; the key of a flow includes the
key of the one that ran before it at the same level. `npx --no-install --offline localci-playwright flows` lists the flows a suite
declares, `localci flows` how they perform.

### Checkpoints

With `checkpoint.browser` the cookies and local storage of the test's browser context are kept, with
`checkpoint.db` a snapshot of the shard's database. The next call with the same key restores them instead of
running the flow. The key is a hash of the flow's name and source (its code, declaration and the file it is
declared in), its inputs, the commits under test, the database template and the key of the flow before it, so
a checkpoint never crosses an edit or a commit. Checkpoints are kept in localci's data directory (owner only),
per suite, up to `suites.<name>.checkpoints.max_size` (default `5GiB`); the least recently used go first.

Database checkpoints are restored only when every shard has its own database (`isolation: isolated`); in any
other mode a flow with `checkpoint.db` just runs, and `flow.end.reason` says so. When `verify` throws on a
restored checkpoint the flow runs for real and the checkpoint is saved again.

### Flow hooks

Bodies and answers are JSON. Errors are `{"error": "…"}` with a 4xx or 5xx status.

| Call | Body | Answer |
|---|---|---|
| `POST …/hooks/flow/lookup` | `flow`, `source` (hash), `inputs` (hash), `parent` (key or `""`), `browser`, `db` | `key`, `hit` (a checkpoint can be restored), `save` (send one after running), `reason?` |
| `POST …/hooks/flow/save` | `key`, `flow`, `browser`, `db`, `storageState`, `outputs`, `replace?` | `saved`, `reason?` |
| `POST …/hooks/flow/restore` | `key`, or `key` + `reset: true` + `parent` | `outputs`, `storageState`; with `reset` the shard's database goes back to what the flow started from |

`restore` for a database checkpoint drops the shard's database and makes it again from the snapshot, so a
process connected to it must reconnect. A suite whose database is not Postgres declares `create`, `drop` and
`snapshot` commands in its config.

### Memoized prefixes

Tests that start with the same flows share that prefix: the first test to need a step of it builds and checkpoints
it, and the tests below it restore. localci learns each test's prefix (the flows it calls before its first step) from
earlier runs, so the first run of a suite, or of a new test, runs unplanned. A wrong prediction only means a test runs
the flows it calls. Results are the same as a run without memoization.

| Call | Query | Answer |
|---|---|---|
| `POST …/hooks/memo/next` | `shard=<n>` | `tests` (ids to run as one batch), `wait` (nothing is ready yet: ask again), `done` |

Asking for work releases the shard's previous batch. The call changes what is handed out, so it is a POST with the
run's hook token. Shards of the local machine use the same plan in process. `suites.<name>.memo: false` turns memoization and learning off for a suite (the baseline when measuring what it saves). Memoization needs `isolation: isolated`;
under `shared-db` and `tenant` the tests keep their shards and only per-test browser checkpoints are restored.

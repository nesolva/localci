# Flows and checkpoints

Most end-to-end suites start every test the same way: create a tenant, sign in, open a page. A flow is that setup
written once, with declared inputs and outputs. localci can then save the result (the browser's cookies and storage,
and a snapshot of the database) and give it back to the next test that asks for the same thing, in milliseconds
instead of seconds.

```ts
// flows/login.ts
import { defineFlow } from "@nesolva/localci-playwright";

export const login = defineFlow({
  name: "login",
  inputs: { slug: "string", password: "string" },
  outputs: { user: "string" },
  checkpoint: { browser: true, db: true },
  run: async ({ page, inputs }) => {
    await page.goto("/login");
    await page.getByLabel("Shop").fill(inputs.slug);
    await page.getByLabel("Password").fill(inputs.password);
    await page.getByRole("button", { name: "Sign in" }).click();
    return { user: inputs.slug };
  },
});
```

```ts
// tests/cart.spec.ts
import { test } from "@nesolva/localci-playwright";

test("adds an item", async ({ page, flow }) => {
  await flow("login", { slug: "acme", password: "demo" });
  // ...
});
```

The sample in [`examples/acme-shop`](../examples/acme-shop) has three flows and a spec that uses them.

## What an agent sees

`localci flows` prints one line per flow: its inputs and outputs, how many tests use it, how long it takes and how
often a checkpoint was restored. The [skill](agents.md) tells agents to run it before writing setup steps, so they
call the flow that exists instead of writing the steps again. The dashboard's run page has a DAG tab that shows which
flows a run used and which were restored.

## When a checkpoint is reused

A checkpoint is found again only when everything that could change its result is the same: the flow's name and
source, its inputs, the commits under test, the database template and the flow that ran before it. Edit the flow, or
commit anything, and the next run builds a new one. Checkpoints live in localci's data directory, readable only by
your user, up to 5 GiB per suite by default (`suites.<name>.checkpoints.max_size`); the least recently used go first.

A flow can have a `verify` function. When it throws on a restored checkpoint (a session that expired, say) the flow
runs for real and the checkpoint is saved again.

Database checkpoints are restored only when every shard has its own database (`isolation: isolated`, see
[isolation levels](isolation.md)). In the other levels a flow with `checkpoint.db` simply runs, and the report says
why. Browser checkpoints still restore.

## Memoized prefixes

Tests that start with the same flows share that start: the first test that needs a step builds and saves it, the
others restore it. localci learns each test's opening flows from earlier runs, so the first run of a suite runs
without the benefit. A wrong guess only means a test runs the flows it calls; results are the same as without
memoization. `suites.<name>.memo: false` turns it off, which is the baseline when you want to measure what it saves.

Flow inputs and outputs are redacted before they reach a report: values under keys such as `password`, `token` or
`session` become `***`, and a browser's storage state never goes into one.

The wire format is in the [protocol](protocol.md#flows).

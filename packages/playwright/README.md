# @nesolva/localci-playwright

Makes a Playwright suite report to localci: tests, steps, artifacts and failure
reports an agent can act on (failing step, expected vs actual, accessibility-tree excerpt, console and
network errors, screenshot and trace paths).

```ts
// playwright.config.ts
import { defineConfig } from "@playwright/test";
import { withLocalci } from "@nesolva/localci-playwright";

export default withLocalci(defineConfig({ /* your config */ }));
```

```ts
// your tests
import { test, expect } from "@nesolva/localci-playwright";
```

- `npx --no-install --offline localci-playwright list` prints the suite's tests as NDJSON `{test,file,title,line}`.
- With `LOCALCI_TESTS=<file of test ids>` only those tests run.
- Without `LOCALCI_REPORT` the reporter does nothing, so the same config works outside localci.

Requires Node 20+ and `@playwright/test` 1.49 or later. The protocol is described in
[`docs/protocol.md`](../../docs/protocol.md). MIT.

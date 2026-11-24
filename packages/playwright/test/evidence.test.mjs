import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { createRequire } from "node:module";
import { tmp } from "./helpers.mjs";

const require = createRequire(import.meta.url);
const Reporter = require("../dist/reporter.js").default;

/** Drives the reporter the way Playwright does and returns the events it wrote. */
function report(outcome, result, attachments = []) {
  const file = path.join(tmp("rep"), "report.ndjson");
  process.env.LOCALCI_REPORT = file;
  const r = new Reporter();
  r.onBegin({ rootDir: "/p" }, { allTests: () => [] });
  const t = {
    title: "t",
    location: { file: "/p/a.spec.ts", line: 1 },
    parent: { project: () => ({ name: "chromium" }) },
    titlePath: () => ["", "chromium", "a.spec.ts", "t"],
    outcome: () => outcome,
  };
  // Playwright always gives a result its `errors` list (empty when the test passed).
  r.onTestEnd(t, { duration: 1, retry: 0, steps: [], attachments, errors: result.error ? [result.error] : [], ...result });
  delete process.env.LOCALCI_REPORT;
  return fs.readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l)).find((e) => e.t === "test.end");
}

test("a test.fail() test that fails is reported as passed, and one that unexpectedly passes as failed", () => {
  const expected = report("expected", { status: "failed", error: { message: "boom" } });
  assert.equal(expected.status, "passed");
  assert.equal(expected.failure, undefined);
  const timedOut = report("expected", { status: "timedOut", error: { message: "slow" } });
  assert.equal(timedOut.status, "passed");

  const unexpected = report("unexpected", { status: "passed" });
  assert.equal(unexpected.status, "failed");
  assert.ok(unexpected.failure.message.includes("fail"));

  const real = report("unexpected", { status: "failed", error: { message: "boom" } });
  assert.equal(real.status, "failed");
  assert.equal(real.failure.message, "boom");
  assert.equal(report("skipped", { status: "skipped" }).status, "skipped");
});

test("failure evidence leaves no query strings and no long text", () => {
  const body = JSON.stringify({
    console: ["x".repeat(5000)],
    network: [{ method: "GET", url: "https://api.acme.test/cb?code=SECRET&token=abc#frag", status: 401 }],
    aria: null,
  });
  const ev = report(
    "unexpected",
    { status: "failed", error: { message: "boom" } },
    [{ name: "localci-context", body: Buffer.from(body) }],
  );
  assert.equal(ev.failure.network[0].url, "https://api.acme.test/cb");
  assert.ok(ev.failure.console[0].length <= 501);
  assert.ok(!JSON.stringify(ev).includes("SECRET"));
});

test("the fixture-side helper keeps origin and path only", () => {
  const { safeUrl } = require("../dist/failure-context.js");
  assert.equal(safeUrl("http://u:pw@h.test:8080/a/b?q=1#x"), "http://h.test:8080/a/b");
  assert.equal(safeUrl("/rel?token=1"), "/rel");
});

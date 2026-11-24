import assert from "node:assert/strict";
import { test } from "node:test";
import { reportedStatus } from "../dist/reporter.js";

// `test.fail()` declares a known bug: failing is what the test expects. When it passes on one attempt and fails on
// its retry, Playwright calls the outcome `flaky` and the final attempt is the expected failure: a pass, not a red.
test("a test.fail() that fails as declared only on a retry is reported passed", () => {
  assert.equal(reportedStatus("flaky", "failed", "failed"), "passed");
  assert.equal(reportedStatus("flaky", "timedOut", "timedOut"), "passed");
});

test("a flaky test that failed on the way still reports each attempt as it ended", () => {
  assert.equal(reportedStatus("flaky", "passed", "passed"), "passed");
  assert.equal(reportedStatus("unexpected", "failed", "passed"), "failed");
  assert.equal(reportedStatus("unexpected", "timedOut", "passed"), "timedout");
  // The first attempt of a test.fail() that passed is a failure of what it declared.
  assert.equal(reportedStatus("unexpected", "passed", "failed"), "failed");
  assert.equal(reportedStatus("expected", "failed", "failed"), "passed");
});

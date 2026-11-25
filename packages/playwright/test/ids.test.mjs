import assert from "node:assert/strict";
import * as fs from "node:fs";
import { test } from "node:test";
import { here } from "./helpers.mjs";
import { testId } from "../dist/ids.js";
import * as path from "node:path";

// The pytest package computes the same ids from the same file: `protocol/test-ids.json`. If this function changes,
// regenerate the file from it and both packages' tests say whether they still agree.
const cases = JSON.parse(fs.readFileSync(path.resolve(here, "../../../protocol/test-ids.json"), "utf8"));

test("ids match the shared fixture the other packages check", () => {
  assert.ok(cases.length > 10);
  for (const c of cases) assert.equal(testId(c.project, c.file, c.titles), c.id, JSON.stringify(c));
});

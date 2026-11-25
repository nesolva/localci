import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { createRequire } from "node:module";
import { here, tmp } from "./helpers.mjs";

// A shard runs the tests whose ids are in $LOCALCI_TESTS and reports each with the id the listing gave it.
// The fixture's tests need no browser.
const require = createRequire(import.meta.url);
const { listTests } = require("../dist/list.js");
const fixture = path.join(here, "fixtures", "selection");
const playwright = require.resolve("@playwright/test/cli", { paths: [fixture] });

const list = (config) => listTests(["-c", config], fixture);
const pick = (listed, project, title) => {
  const t = listed.find((l) => l.project === project && l.title === title);
  assert.ok(t, `listed: ${project} ${title}`);
  return t;
};

/** Runs the fixture suite as a shard given `tests` and returns what the reporter reported, sorted by id. */
function runShard(config, tests) {
  const dir = tmp("selection");
  const ids = path.join(dir, "tests.txt");
  const report = path.join(dir, "report.ndjson");
  fs.writeFileSync(ids, tests.map((t) => t.test).join("\n") + "\n");
  const r = spawnSync(process.execPath, [playwright, "test", "-c", config], {
    cwd: fixture,
    encoding: "utf8",
    env: { ...process.env, CI: "", LOCALCI_TESTS: ids, LOCALCI_REPORT: report, LOCALCI_OUTPUT_DIR: path.join(dir, "out") },
  });
  const events = fs.existsSync(report) ? fs.readFileSync(report, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l)) : [];
  const ran = events.filter((e) => e.t === "test.begin").map((e) => ({ test: e.test, title: e.title }));
  return { ran: ran.sort((a, b) => (a.test < b.test ? -1 : 1)), output: r.stdout + r.stderr };
}

const given = (tests) => tests.map((t) => ({ test: t.test, title: t.title })).sort((a, b) => (a.test < b.test ? -1 : 1));

test("a shard runs the tagged tests it was given: config, describe and test tags", () => {
  const listed = list("tagged.config.js");
  const tests = [pick(listed, "", "plain"), pick(listed, "", "tagged"), pick(listed, "", "checkout › refunds")];
  const { ran, output } = runShard("tagged.config.js", tests);
  assert.deepEqual(ran, given(tests), output.slice(-800));
});

import Ajv from "ajv";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { createRequire } from "node:module";
import { runSample, schemaPath, tmp, sampleDir, startApp, freePort } from "./helpers.mjs";

const require = createRequire(import.meta.url);
const validate = new Ajv({ allErrors: true }).compile(JSON.parse(fs.readFileSync(schemaPath, "utf8")));

// One broken run is shared by the tests below: it is the slow part.
let broken;
const brokenRun = () => (broken ??= runSample({ env: { ACME_BREAK: "1" } }));

test("a broken run emits events that all validate against the schema", async () => {
  const { events, status } = await brokenRun();
  assert.equal(status, 1, "the broken sample suite fails");
  assert.ok(events.length > 50);
  for (const e of events) assert.ok(validate(e), `${JSON.stringify(e).slice(0, 200)}\n${JSON.stringify(validate.errors)}`);
  assert.equal(events[0].t, "begin");
  assert.equal(events.at(-1).t, "end");
  assert.equal(events.at(-1).status, "failed");
  const begun = events.filter((e) => e.t === "test.begin").length;
  assert.equal(begun, events.filter((e) => e.t === "test.end").length);
  assert.equal(begun, events[0].total);
});

test("the failing test carries an agent-ready failure report", async () => {
  const { events } = await brokenRun();
  const failed = events.filter((e) => e.t === "test.end" && e.status === "failed");
  assert.equal(failed.length, 1);
  const f = failed[0].failure;
  assert.match(f.step, /check the total/);
  assert.equal(f.expected, '"Total: $27.50"');
  assert.equal(f.actual, '"Total: $17.50"');
  assert.match(f.location, /cart\.spec\.ts:\d+$/);
  const lines = f.aria.split("\n");
  assert.ok(lines.length <= 40);
  assert.ok(f.aria.includes("Total: $17.50"), "the excerpt contains the target");
  assert.ok(f.console.some((c) => c.includes("cart total mismatch")));
  assert.ok(f.network.some((n) => n.status === 422 && n.method === "POST" && n.url.endsWith("/api/mismatch")));
  assert.ok(fs.existsSync(f.screenshot));
  assert.ok(fs.existsSync(f.trace));
  // every artifact event points at a file that exists
  for (const a of events.filter((e) => e.t === "artifact")) assert.ok(fs.existsSync(a.path), a.path);
});

test("steps nest: the failing test.step has children and every step ends", async () => {
  const { events } = await brokenRun();
  const begins = events.filter((e) => e.t === "step.begin");
  const ends = new Map(events.filter((e) => e.t === "step.end").map((e) => [e.step, e]));
  assert.ok(begins.length > 10);
  for (const b of begins) assert.ok(ends.has(b.step), `step ${b.title} never ended`);
  const named = begins.find((b) => b.title === "check the total" && b.kind === "step");
  assert.ok(named, "test.step shows as a step");
  assert.equal(ends.get(named.step).ok, false);
  assert.ok(begins.some((b) => b.parent === named.step && b.kind === "expect"));
});

test("$LOCALCI_TESTS with two ids runs exactly those two tests", async () => {
  const cli = path.resolve(path.dirname(require.resolve("../package.json")), "dist/cli.js");
  const { spawnSync } = await import("node:child_process");
  const list = spawnSync("node", [cli, "list"], { cwd: sampleDir, encoding: "utf8" });
  assert.equal(list.status, 0, list.stderr);
  const all = list.stdout.split("\n").filter(Boolean).map((l) => JSON.parse(l));
  assert.ok(all.length >= 10 && all.length <= 15, `${all.length} tests`);
  for (const t of all) assert.deepEqual(Object.keys(t), ["test", "file", "title", "line"]);
  const picked = [all[1], all.at(-1)]; // two tests from different files
  const ids = path.join(tmp("ids"), "tests.txt");
  fs.writeFileSync(ids, picked.map((t) => t.test).join("\n") + "\n");
  const { events, status, output } = await runSample({ env: { LOCALCI_TESTS: ids } });
  assert.equal(status, 0, output.slice(-500));
  const ran = events.filter((e) => e.t === "test.begin");
  assert.deepEqual(ran.map((e) => e.test).sort(), picked.map((t) => t.test).sort());
  assert.deepEqual(ran.map((e) => e.title).sort(), picked.map((t) => t.title).sort());
});

test("two sample instances with their own port and database share no state", async () => {
  const a = { port: await freePort(), db: path.join(tmp("a"), "db.json") };
  const b = { port: await freePort(), db: path.join(tmp("b"), "db.json") };
  const apps = [await startApp(a.port, a.db), await startApp(b.port, b.db)];
  try {
    const made = await fetch(`http://127.0.0.1:${a.port}/api/tenants`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name: "Only In A" }),
    });
    const { slug } = await made.json();
    assert.equal((await fetch(`http://127.0.0.1:${a.port}/t/${slug}/`)).status, 200);
    assert.equal((await fetch(`http://127.0.0.1:${b.port}/t/${slug}/`)).status, 404);
    assert.ok(fs.existsSync(a.db));
    assert.ok(!fs.existsSync(b.db), "the other instance wrote nothing");
  } finally {
    for (const app of apps) app.kill();
  }
});

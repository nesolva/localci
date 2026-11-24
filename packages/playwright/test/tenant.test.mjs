import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { defineFlow } from "../dist/index.js";
import { acquire, tenantFixture } from "../dist/tenant.js";
import { tmp } from "./helpers.mjs";

const fx = { page: {}, context: {}, request: {} };
const info = (dir) => ({ project: { name: "chromium" }, config: { rootDir: dir }, file: path.join(dir, "a.spec.ts"), titlePath: ["", "a test"] });

async function fixture(dir) {
  let got = "unset";
  await tenantFixture(fx, async (t) => void (got = t), info(dir));
  return got;
}

function withEnv(env, fn) {
  const saved = {};
  for (const k of Object.keys(env)) saved[k] = process.env[k];
  Object.assign(process.env, env);
  return fn().finally(() => {
    for (const [k, v] of Object.entries(saved)) v === undefined ? delete process.env[k] : (process.env[k] = v);
  });
}

test("the tenant flow runs once per test command, however many workers start", async () => {
  const dir = tmp("tenant");
  let runs = 0;
  const seen = [];
  defineFlow({
    name: "make-tenant-once",
    inputs: { shard: "number" },
    outputs: { slug: "string" },
    // A checkpoint must never make the flow skip: it would hand this shard another shard's tenant.
    checkpoint: { browser: true, db: true },
    run: async ({ inputs }) => {
      runs++;
      seen.push(inputs);
      return { slug: `shard-${inputs.shard}` };
    },
  });
  await withEnv({ LOCALCI_TENANT_FLOW: "make-tenant-once", LOCALCI_OUTPUT_DIR: dir, LOCALCI_SHARD: "3", LOCALCI_RUN: "r1" }, async () => {
    assert.deepEqual(await fixture(dir), { slug: "shard-3" });
    assert.deepEqual(await fixture(dir), { slug: "shard-3" }); // a restarted worker
  });
  assert.equal(runs, 1);
  assert.deepEqual(seen, [{ shard: 3 }]); // only the inputs the flow declares
});

test("every test command of a shard shares the tenant the core's tenant file records", async () => {
  const shared = path.join(tmp("tenant-shard"), "shard-0.tenant.json");
  let runs = 0;
  defineFlow({ name: "make-tenant-per-shard", outputs: { slug: "string" }, run: async () => (runs++, { slug: "t0" }) });
  for (const out of [tmp("serial"), tmp("rerun1")]) {
    await withEnv({ LOCALCI_TENANT_FLOW: "make-tenant-per-shard", LOCALCI_TENANT_FILE: shared, LOCALCI_OUTPUT_DIR: out, LOCALCI_SHARD: "0", LOCALCI_RUN: "r1" }, async () => {
      assert.deepEqual(await fixture(out), { slug: "t0" });
    });
  }
  assert.equal(runs, 1, "a second test command on the same stack must not make the tenant again");
});

test("a failed tenant flow is written for the core and not retried by later tests", async () => {
  const dir = tmp("tenant");
  let runs = 0;
  defineFlow({
    name: "make-tenant-broken",
    run: async () => {
      runs++;
      throw new Error("POST /api/tenants answered 500\nstack line");
    },
  });
  await withEnv({ LOCALCI_TENANT_FLOW: "make-tenant-broken", LOCALCI_OUTPUT_DIR: dir }, async () => {
    for (let i = 0; i < 2; i++) {
      await assert.rejects(fixture(dir), /tenant flow "make-tenant-broken" failed: POST \/api\/tenants answered 500$/);
    }
  });
  assert.equal(runs, 1);
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(dir, "tenant.json"), "utf8")), {
    flow: "make-tenant-broken",
    error: "POST /api/tenants answered 500",
  });
});

test("a flow nobody declared fails the shard with a hint, and without tenant isolation the fixture is inert", async () => {
  const dir = tmp("tenant");
  await withEnv({ LOCALCI_TENANT_FLOW: "never-declared", LOCALCI_OUTPUT_DIR: dir }, async () => {
    await assert.rejects(fixture(dir), /no flow named "never-declared" \(is its file imported by a test\?\)/);
  });
  await withEnv({ LOCALCI_TENANT_FLOW: "", LOCALCI_OUTPUT_DIR: dir }, async () => {
    assert.equal(await fixture(tmp("tenant")), undefined);
  });
});

test("two workers of one shard starting together make the tenant once and share it", async () => {
  const dir = tmp("tenant-workers");
  const worker = fileURLToPath(new URL("./tenant-worker.mjs", import.meta.url));
  const env = { ...process.env, WORKER_DIR: dir, LOCALCI_TENANT_FLOW: "make-tenant-shared", LOCALCI_TENANT_FILE: path.join(dir, "shard-1.tenant.json"), LOCALCI_SHARD: "1" };
  const run = () =>
    new Promise((resolve, reject) => {
      const child = spawn(process.execPath, [worker], { env, stdio: ["ignore", "pipe", "inherit"] });
      let out = "";
      child.stdout.on("data", (d) => (out += d));
      child.on("exit", (code) => (code === 0 ? resolve(JSON.parse(out)) : reject(new Error(`worker exited ${code}`))));
    });
  const [a, b] = await Promise.all([run(), run()]);
  const made = fs.readFileSync(path.join(dir, "flow.log"), "utf8").split("\n").filter(Boolean);
  assert.equal(made.length, 1, `the tenant flow ran ${made.length} times`);
  assert.deepEqual(a, b);
  assert.ok(!fs.existsSync(path.join(dir, "shard-1.tenant.json.lock")), "the lock is released");
});

test("a lock left by a worker that died does not hold the shard", async () => {
  const dir = tmp("tenant-stale");
  const file = path.join(dir, "shard-1.tenant.json");
  fs.writeFileSync(`${file}.lock`, "999999999"); // no such process
  let runs = 0;
  defineFlow({ name: "make-tenant-after-crash", outputs: { slug: "string" }, run: async () => (runs++, { slug: "t" }) });
  await withEnv({ LOCALCI_TENANT_FLOW: "make-tenant-after-crash", LOCALCI_TENANT_FILE: file, LOCALCI_SHARD: "1" }, async () => {
    assert.deepEqual(await fixture(dir), { slug: "t" });
  });
  assert.equal(runs, 1);
});

test("releasing a lock that a takeover replaced leaves the new owner's lock alone", async () => {
  const dir = tmp("tenant-release");
  const file = path.join(dir, "shard-1.tenant.json");
  const got = await acquire(file, "any-flow");
  assert.ok(got.release);
  fs.writeFileSync(`${file}.lock`, "4242"); // another worker took over
  got.release();
  assert.equal(fs.readFileSync(`${file}.lock`, "utf8"), "4242");
});

test("a dead worker's lock is not removed while another worker is taking it over", async () => {
  const dir = tmp("tenant-takeover-guard");
  const file = path.join(dir, "shard-1.tenant.json");
  fs.writeFileSync(`${file}.lock`, "999999999"); // its owner is gone
  fs.writeFileSync(`${file}.lock.takeover`, ""); // and a worker is in the middle of taking it over
  let got;
  const pending = acquire(file, "any-flow").then((g) => (got = g));
  await new Promise((r) => setTimeout(r, 300));
  assert.equal(got, undefined, "a second worker went on while the takeover was in progress");
  assert.equal(fs.readFileSync(`${file}.lock`, "utf8"), "999999999", "the lock was removed under the worker taking it over");
  fs.rmSync(`${file}.lock.takeover`); // the first worker is done
  const result = await pending;
  assert.ok(result.release);
  result.release();
});

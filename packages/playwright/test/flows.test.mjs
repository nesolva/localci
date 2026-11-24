import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";
import { test } from "node:test";
import { createFlowCaller, defineFlow, redact, SENSITIVE_KEYS } from "../dist/index.js";
import { tmp } from "./helpers.mjs";

const KEY = "a".repeat(64);

/** A core that answers the three flow hooks; `state` says what lookup returns. */
function fakeCore(state = {}) {
  const calls = [];
  const fetch = async (url, init) => {
    const action = new URL(url).pathname.split("/").pop();
    const body = JSON.parse(init.body);
    calls.push({ action, body, headers: init.headers });
    const answers = {
      lookup: { key: KEY, hit: !!state.hit, save: state.save ?? true },
      save: { saved: true },
      restore: body.reset ? { reset: state.reset ?? true } : { outputs: state.outputs ?? { user: "demo" }, storageState: { cookies: [{ name: "sid", value: "s3cret", domain: "x", path: "/" }] } },
    };
    if (action === "restore" && state.gone && !body.reset) return new Response(JSON.stringify({ error: "no checkpoint for this key" }), { status: 404 });
    return new Response(JSON.stringify(answers[action]), { status: 200 });
  };
  return { calls, fetch };
}

function fakeContext() {
  const log = [];
  return {
    log,
    context: {
      storageState: async () => ({ cookies: [{ name: "sid", value: "s3cret" }], origins: [] }),
      addCookies: async (c) => log.push(["addCookies", c.length]),
      addInitScript: async (_fn, arg) => log.push(["addInitScript", arg?.origin]),
      clearCookies: async () => log.push(["clearCookies"]),
    },
  };
}

function caller(core, extra = {}) {
  const dir = tmp("flows");
  const report = path.join(dir, "report.ndjson");
  const { context, log } = fakeContext();
  const flow = createFlowCaller({ test: "t1", page: {}, context, request: {}, api: "http://core/runs/1", report, fetch: core.fetch, ...extra });
  const events = () => fs.readFileSync(report, "utf8").trim().split("\n").map((l) => JSON.parse(l));
  return { flow, events, log };
}

test("a miss runs the flow, saves a checkpoint and redacts what the events show", async () => {
  const core = fakeCore();
  let ran = 0;
  defineFlow({ name: "n-login", inputs: { user: "string", password: "string" }, outputs: { user: "string" }, checkpoint: { browser: true, db: true }, run: async ({ inputs }) => (ran++, { user: inputs.user }) });
  const { flow, events } = caller(core);
  const out = await flow("n-login", { user: "demo", password: "hunter2" });
  assert.deepEqual(out, { user: "demo" });
  assert.equal(ran, 1);
  const save = core.calls.find((c) => c.action === "save");
  assert.equal(save.body.key, KEY);
  assert.equal(save.body.replace, false);
  assert.ok(save.body.storageState.cookies, "the storage state goes to the core");

  const text = JSON.stringify(events());
  assert.ok(!text.includes("hunter2") && !text.includes("s3cret"), "no secret or storage state in an event");
  const begin = events().find((e) => e.t === "flow.begin");
  const end = events().find((e) => e.t === "flow.end");
  assert.equal(begin.inputs.password, "***");
  assert.equal(begin.inputs.user, "demo");
  assert.equal(end.checkpoint, "saved");
  assert.equal(typeof end.ms, "number");
});

test("a hit restores the checkpoint without running the flow", async () => {
  const core = fakeCore({ hit: true, outputs: { user: "restored" } });
  let ran = 0;
  defineFlow({ name: "n-restore", outputs: { user: "string" }, checkpoint: { browser: true }, run: async () => (ran++, { user: "ran" }) });
  const { flow, events, log } = caller(core);
  assert.deepEqual(await flow("n-restore"), { user: "restored" });
  assert.equal(ran, 0);
  assert.deepEqual(log[0], ["addCookies", 1]);
  assert.equal(events().find((e) => e.t === "flow.end").checkpoint, "restored");
  assert.ok(!core.calls.some((c) => c.action === "save"));
});

test("a failing verify runs the flow again, resets the database and saves a replacement", async () => {
  const core = fakeCore({ hit: true });
  let ran = 0;
  defineFlow({
    name: "n-verify",
    outputs: { user: "string" },
    checkpoint: { browser: true, db: true },
    verify: async () => {
      throw new Error("session expired");
    },
    run: async () => (ran++, { user: "fresh" }),
  });
  const { flow, events, log } = caller(core);
  assert.deepEqual(await flow("n-verify"), { user: "fresh" });
  assert.equal(ran, 1);
  const reset = core.calls.find((c) => c.action === "restore" && c.body.reset);
  assert.ok(reset, "the database is put back before the flow runs again");
  assert.ok(log.some((l) => l[0] === "clearCookies"));
  const save = core.calls.find((c) => c.action === "save");
  assert.equal(save.body.replace, true);
  const end = events().find((e) => e.t === "flow.end");
  assert.equal(end.checkpoint, "saved");
  assert.match(end.reason, /failed verify.*session expired/);
});

test("a database flow the core could not put back runs again but is not saved", async () => {
  const core = fakeCore({ hit: true, reset: false });
  let ran = 0;
  defineFlow({
    name: "n-noreset",
    outputs: { user: "string" },
    checkpoint: { db: true },
    verify: async () => {
      throw new Error("stale");
    },
    run: async () => (ran++, { user: "fresh" }),
  });
  const { flow } = caller(core);
  assert.deepEqual(await flow("n-noreset"), { user: "fresh" });
  assert.equal(ran, 1);
  assert.ok(!core.calls.some((c) => c.action === "save"), "a snapshot of a database in an unknown state is never saved");
});

test("a hit whose restore fails puts a database flow's database back before running it", async () => {
  const core = fakeCore({ hit: true, gone: true });
  let ran = 0;
  defineFlow({ name: "n-gone", outputs: { user: "string" }, checkpoint: { db: true }, run: async () => (ran++, { user: "fresh" }) });
  const { flow } = caller(core);
  assert.deepEqual(await flow("n-gone"), { user: "fresh" });
  assert.equal(ran, 1);
  const order = core.calls.map((c) => c.action + (c.body.reset ? ":reset" : ""));
  assert.deepEqual(order, ["lookup", "restore", "restore:reset", "save"], "the reset comes before the flow runs, then the flow is saved");
});

test("a hit whose restore fails and whose reset fails runs the database flow but does not save it", async () => {
  const core = fakeCore({ hit: true, gone: true, reset: false });
  defineFlow({ name: "n-gone2", outputs: { user: "string" }, checkpoint: { db: true }, run: async () => ({ user: "fresh" }) });
  const { flow } = caller(core);
  await flow("n-gone2");
  assert.ok(!core.calls.some((c) => c.action === "save"));
});

test("without a core the flow just runs and the hook token is sent when there is one", async () => {
  defineFlow({ name: "n-alone", outputs: { ok: "boolean" }, checkpoint: { browser: true }, run: async () => ({ ok: true }) });
  const down = { calls: [], fetch: async () => { throw new Error("connection refused"); } };
  const a = caller(down);
  assert.deepEqual(await a.flow("n-alone"), { ok: true });
  assert.equal(a.events().find((e) => e.t === "flow.end").checkpoint, "none");

  const core = fakeCore();
  const b = caller(core, { token: "tok" });
  await b.flow("n-alone");
  assert.equal(core.calls[0].headers.authorization, "Bearer tok");
});

test("a flow that does not return a declared output fails, and its end event says so", async () => {
  defineFlow({ name: "n-missing", outputs: { slug: "string" }, run: async () => ({}) });
  const { flow, events } = caller(fakeCore());
  await assert.rejects(flow("n-missing"), /declares the output "slug"/);
  const end = events().find((e) => e.t === "flow.end");
  assert.equal(end.checkpoint, "none");
  assert.ok(end.reason);
});

test("a flow called inside another nests under the outer step and chains its key on the outer one's", async () => {
  const core = fakeCore();
  defineFlow({ name: "n-inner", checkpoint: { browser: true }, run: async () => ({}) });
  defineFlow({ name: "n-outer", checkpoint: { browser: true }, run: async ({ flow }) => (await flow("n-inner"), {}) });
  const { flow, events } = caller(core);
  await flow("n-outer");
  const steps = events().filter((e) => e.t === "step.begin");
  assert.equal(steps.length, 2);
  assert.equal(steps[1].parent, steps[0].step);
  const lookups = core.calls.filter((c) => c.action === "lookup");
  assert.equal(lookups[0].body.parent, "");
  assert.equal(lookups[1].body.parent, KEY, "the inner flow's key chains on the enclosing flow's key");
});

test("editing a flow's code changes its source hash, which is part of the key", () => {
  const a = defineFlow({ name: "n-src", run: async () => ({ v: 1 }) });
  const b = defineFlow({ name: "n-src", run: async () => ({ v: 2 }) });
  assert.notEqual(a.source, b.source);
});

test("redact hides secrets at any depth and cuts long strings", () => {
  const out = redact({ password: "x", nested: { apiKey: "k", list: [{ cookie: "c" }], ok: "fine" }, long: "a".repeat(300) });
  assert.equal(out.password, "***");
  assert.equal(out.nested.apiKey, "***");
  assert.equal(out.nested.list[0].cookie, "***");
  assert.equal(out.nested.ok, "fine");
  assert.ok(out.long.length <= 201);
});

test("redact hides session, auth, jwt and pwd names, and the list equals the shared one", () => {
  const out = redact({ sessionId: "a", Authorization: "Bearer b", jwt: "c", pwd: "d", user: "demo" });
  assert.deepEqual(out, { sessionId: "***", Authorization: "***", jwt: "***", pwd: "***", user: "demo" });
  const shared = JSON.parse(fs.readFileSync(new URL("../../../protocol/sensitive-keys.json", import.meta.url), "utf8"));
  assert.deepEqual(SENSITIVE_KEYS, shared.keys);
});

test("a checkpoint saved on one shard restores its local storage on a shard with another port", async () => {
  const saved = { cookies: [], origins: [{ origin: "http://127.0.0.1:3001", localStorage: [{ name: "t", value: "v" }] }, { origin: "http://127.0.0.1:9000", localStorage: [{ name: "other", value: "v" }] }] };
  let stored;
  const saveCore = {
    fetch: async (url, init) => {
      const action = new URL(url).pathname.split("/").pop();
      const body = JSON.parse(init.body);
      if (action === "save") stored = body.storageState;
      const answers = { lookup: { key: KEY, hit: false, save: true }, save: { saved: true } };
      return new Response(JSON.stringify(answers[action]), { status: 200 });
    },
  };
  defineFlow({ name: "n-shard", checkpoint: { browser: true }, run: async () => ({}) });
  // the fake context reports a storage state with the saving shard's origins
  const ctx = fakeContext();
  ctx.context.storageState = async () => saved;
  await createFlowCaller({ test: "t1", page: {}, context: ctx.context, request: {}, api: "http://core/runs/1", fetch: saveCore.fetch, baseURL: "http://127.0.0.1:3001" })("n-shard");
  assert.equal(stored.appOrigin, "http://127.0.0.1:3001");

  const restoreCore = {
    fetch: async (url) => {
      const action = new URL(url).pathname.split("/").pop();
      const answers = { lookup: { key: KEY, hit: true, save: true }, restore: { outputs: {}, storageState: stored } };
      return new Response(JSON.stringify(answers[action]), { status: 200 });
    },
  };
  const two = fakeContext();
  await createFlowCaller({ test: "t2", page: {}, context: two.context, request: {}, api: "http://core/runs/1", fetch: restoreCore.fetch, baseURL: "http://127.0.0.1:3002" })("n-shard");
  const origins = two.log.filter((l) => l[0] === "addInitScript").map((l) => l[1]);
  assert.deepEqual(origins, ["http://127.0.0.1:3002", "http://127.0.0.1:9000"], "the app's storage follows the shard, other origins stay exact");

  const three = fakeContext();
  await createFlowCaller({ test: "t3", page: {}, context: three.context, request: {}, api: "http://core/runs/1", fetch: restoreCore.fetch, baseURL: "http://other-host:3002" })("n-shard");
  assert.deepEqual(three.log.filter((l) => l[0] === "addInitScript").map((l) => l[1]), ["http://127.0.0.1:3001", "http://127.0.0.1:9000"], "another host gets nothing applied by origin");
});

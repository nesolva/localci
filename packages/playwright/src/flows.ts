import type { APIRequestContext, BrowserContext, Page, TestInfo } from "@playwright/test";
import { createHash } from "node:crypto";
import * as fs from "node:fs";
import { relFile, testId } from "./ids";

/** What a flow input or output is: a JSON string, number, boolean, or any other JSON value. */
export type FlowType = "string" | "number" | "boolean" | "json";
type TypeOf<K> = K extends "string" ? string : K extends "number" ? number : K extends "boolean" ? boolean : unknown;
export type FlowValues<S extends Record<string, FlowType>> = { [K in keyof S]: TypeOf<S[K]> };

export interface FlowArgs<I extends Record<string, FlowType>> {
  page: Page;
  context: BrowserContext;
  request: APIRequestContext;
  inputs: FlowValues<I>;
  /** Calls another flow from inside this one. */
  flow: FlowCaller;
}

export interface FlowDefinition<I extends Record<string, FlowType>, O extends Record<string, FlowType>> {
  name: string;
  inputs?: I;
  outputs?: O;
  /**
   * What a finished run of the flow keeps so that later calls skip it: `browser` the cookies and local storage of the
   * test's context, `db` a snapshot of the shard's database (restored only with isolated shards).
   */
  checkpoint?: { browser?: boolean; db?: boolean };
  /** Runs after a restore and throws when the restored state is no longer good; the flow then runs for real. */
  verify?: (args: FlowArgs<I> & { outputs: FlowValues<O> }) => Promise<void>;
  run: (args: FlowArgs<I>) => Promise<FlowValues<O>>;
}

export interface Flow<I extends Record<string, FlowType> = Record<string, FlowType>, O extends Record<string, FlowType> = Record<string, FlowType>>
  extends FlowDefinition<I, O> {
  /** Hash of everything that decides whether a saved result still applies: the flow's code and its declaration. */
  readonly source: string;
}

export interface FlowCaller {
  <I extends Record<string, FlowType>, O extends Record<string, FlowType>>(flow: Flow<I, O>, inputs: FlowValues<I>): Promise<FlowValues<O>>;
  (name: string, inputs?: Record<string, unknown>): Promise<Record<string, unknown>>;
}

const registry = new Map<string, Flow<any, any>>();
const files = new Map<string, string | undefined>();

const sha = (...parts: string[]) => {
  const h = createHash("sha256");
  for (const p of parts) h.update(p).update("\0");
  return h.digest("hex");
};

/** The file that called `defineFlow`, so editing a helper next to the flow also gives it a new key. */
function callerFile(): string | undefined {
  for (const line of new Error().stack?.split("\n").slice(1) ?? []) {
    const m = /\(?(?:file:\/\/)?(\/[^():]+|[A-Za-z]:[\\/][^():]+):\d+:\d+\)?$/.exec(line.trim());
    if (m && m[1] !== __filename && !m[1].endsWith("/flows.js") && !m[1].endsWith("/flows.ts")) return m[1];
  }
  return undefined;
}

/**
 * Declares a reusable flow: login, create a tenant, anything several tests start with. Tests call it through the
 * `flow` fixture: `await flow("login", { user })`, or `await flow(login, { user })` for typed outputs.
 */
export function defineFlow<I extends Record<string, FlowType> = {}, O extends Record<string, FlowType> = {}>(
  def: FlowDefinition<I, O>,
): Flow<I, O> {
  const file = callerFile();
  let text = "";
  try {
    if (file) text = fs.readFileSync(file, "utf8");
  } catch {
    // the source of the function alone still decides the key
  }
  const flow: Flow<I, O> = {
    ...def,
    source: sha(def.name, String(def.run), String(def.verify ?? ""), JSON.stringify(def.checkpoint ?? {}), JSON.stringify(def.inputs ?? {}), JSON.stringify(def.outputs ?? {}), text),
  };
  // Two files declaring one name is a mistake; the same file loaded again (watch mode) just replaces it.
  if (registry.has(def.name) && files.get(def.name) !== file) throw new Error(`flow "${def.name}" is declared in two files`);
  registry.set(def.name, flow);
  files.set(def.name, file);
  const dump = process.env.LOCALCI_FLOWS_DUMP;
  if (dump) {
    const line = { name: def.name, inputs: def.inputs ?? {}, outputs: def.outputs ?? {}, checkpoint: def.checkpoint ?? {}, file: file ?? null };
    try {
      fs.appendFileSync(dump, JSON.stringify(line) + "\n");
    } catch {
      // listing flows is best effort
    }
  }
  return flow;
}

/** The flows declared so far in this process. */
export function declaredFlows(): Flow<any, any>[] {
  return [...registry.values()];
}

/** Case-insensitive substrings of a name whose value is a secret. The core keeps the same list; both are tested against `protocol/sensitive-keys.json`. */
export const SENSITIVE_KEYS = ["password", "passwd", "pwd", "secret", "token", "cookie", "key", "session", "auth", "jwt", "bearer", "credential"];
const SENSITIVE = new RegExp(SENSITIVE_KEYS.join("|"), "i");
const MAX_STRING = 200;

/** Inputs and outputs as events show them: values under secret-looking keys are `***`, long strings are cut. */
export function redact(v: unknown): unknown {
  if (typeof v === "string") return v.length > MAX_STRING ? v.slice(0, MAX_STRING) + "…" : v;
  if (Array.isArray(v)) return v.map(redact);
  if (v && typeof v === "object") {
    return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, SENSITIVE.test(k) ? "***" : redact(x)]));
  }
  return v;
}

function stable(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(stable).join(",")}]`;
  if (v && typeof v === "object") {
    return `{${Object.entries(v)
      .filter(([, x]) => x !== undefined)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
      .map(([k, x]) => `${JSON.stringify(k)}:${stable(x)}`)
      .join(",")}}`;
  }
  return JSON.stringify(v) ?? "null";
}

interface StorageState {
  cookies?: Parameters<BrowserContext["addCookies"]>[0];
  origins?: { origin: string; localStorage: { name: string; value: string }[] }[];
  /** Origin of the app the state was saved on: the saving shard's base URL (every shard has a port of its own). */
  appOrigin?: string;
}

function originOf(url: string | undefined): string | undefined {
  try {
    return url ? new URL(url).origin : undefined;
  } catch {
    return undefined;
  }
}

/**
 * Puts a saved storage state into a live context: cookies at once, local storage on the next page load. Storage of
 * the app (the origin the state was saved on) goes to this shard's base URL when scheme and host match, whatever
 * port the saving shard had; every other origin is applied where it was saved, exact match only.
 */
async function applyState(context: BrowserContext, state: StorageState | undefined, baseURL?: string) {
  if (!state) return;
  if (state.cookies?.length) await context.addCookies(state.cookies);
  const here = originOf(baseURL);
  for (const o of state.origins ?? []) {
    if (!o.localStorage?.length) continue;
    let target = o.origin;
    if (here && state.appOrigin && o.origin === state.appOrigin) {
      const saved = new URL(state.appOrigin);
      const now = new URL(here);
      if (saved.protocol === now.protocol && saved.hostname === now.hostname) target = here;
    }
    await context.addInitScript(
      ({ origin, items }) => {
        if (location.origin === origin) for (const i of items) localStorage.setItem(i.name, i.value);
      },
      { origin: target, items: o.localStorage },
    );
  }
}

export interface FlowEnv {
  /** Id of the running test. */
  test: string;
  page: Page;
  context: BrowserContext;
  request: APIRequestContext;
  /** The shard's base URL (`use.baseURL`): where restored browser storage is applied. */
  baseURL?: string;
  /** `$LOCALCI_API`; without it flows just run. */
  api?: string;
  /** `$LOCALCI_HOOK_TOKEN`, sent when set. */
  token?: string;
  /** `$LOCALCI_REPORT`. */
  report?: string;
  fetch?: typeof fetch;
}

interface Frame {
  step: string | null;
  /** Key of the flow that finished last at this level: what the next one starts from. */
  last: string;
}

interface Answer {
  ok: boolean;
  body: any;
}

/** Builds the `flow` function of one test. */
export function createFlowCaller(env: FlowEnv): FlowCaller {
  const frames: Frame[] = [{ step: null, last: "" }];
  let seq = 0;
  const doFetch = env.fetch ?? fetch;

  const emit = (t: string, body: Record<string, unknown>) => {
    if (!env.report) return;
    try {
      const fd = fs.openSync(env.report, "a");
      try {
        fs.writeSync(fd, JSON.stringify({ v: 1, t, ts: Date.now(), ...body }) + "\n");
      } finally {
        fs.closeSync(fd);
      }
    } catch {
      // reporting never fails a test
    }
  };

  const hook = async (action: string, body: unknown): Promise<Answer> => {
    if (!env.api) return { ok: false, body: { error: "localci is not running this test" } };
    try {
      const res = await doFetch(`${env.api.replace(/\/$/, "")}/hooks/flow/${action}?test=${encodeURIComponent(env.test)}`, {
        method: "POST",
        headers: { "content-type": "application/json", ...(env.token ? { authorization: `Bearer ${env.token}` } : {}) },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(180_000),
      });
      const text = await res.text();
      let parsed: any = {};
      try {
        parsed = text ? JSON.parse(text) : {};
      } catch {
        // not JSON: keep it as the message
        parsed = { error: text.slice(0, 200) };
      }
      return { ok: res.ok, body: parsed };
    } catch (e) {
      return { ok: false, body: { error: `localci is not reachable: ${(e as Error).message}` } };
    }
  };

  async function call(def: Flow<any, any>, inputs: Record<string, unknown>): Promise<Record<string, unknown>> {
    const frame = frames[frames.length - 1];
    const step = `${env.test}:f${++seq}`;
    const started = Date.now();
    const cp = def.checkpoint ?? {};
    const browser = !!cp.browser;
    const db = !!cp.db;
    const parent = frame.last;
    const hash = sha(stable(inputs));

    const look: Answer | undefined = browser || db
      ? await hook("lookup", { flow: def.name, source: def.source, inputs: hash, parent, browser, db })
      : undefined;
    const key: string = look?.ok && look.body.key ? look.body.key : sha(def.name, def.source, hash, parent);
    let reason: string | undefined = look ? (look.ok ? look.body.reason : look.body.error) : undefined;
    let replace = false;
    // A database flow whose restore failed, or whose reset did not happen, runs on a database in an unknown state:
    // it is not saved.
    let unknownDB = false;

    emit("step.begin", { test: env.test, step, parent: frame.step, title: `flow: ${def.name}`, kind: "flow" });
    emit("flow.begin", { test: env.test, step, flow: def.name, inputs: redact(inputs), key });

    const args = () => ({ page: env.page, context: env.context, request: env.request, inputs, flow: caller });
    let checkpoint: "saved" | "restored" | "none" = "none";
    let outputs: Record<string, unknown> | undefined;
    try {
      if (look?.ok && look.body.hit) {
        const r = await hook("restore", { key });
        if (r.ok) {
          outputs = r.body.outputs ?? {};
          await applyState(env.context, r.body.storageState, env.baseURL);
          if (def.verify) {
            try {
              await def.verify({ ...args(), outputs } as any);
            } catch (e) {
              reason = `the restored checkpoint failed verify (${String((e as Error).message ?? e).split("\n")[0].slice(0, 160)}), the flow ran again`;
              outputs = undefined;
              replace = true;
              if (db) {
                const back = await hook("restore", { key, reset: true, parent });
                if (!back.ok || !back.body.reset) unknownDB = true;
              }
              await env.context.clearCookies();
            }
          }
          if (outputs !== undefined) checkpoint = "restored";
        } else {
          reason = r.body.error;
          // The lookup said hit, so it did not reset the database as a miss does: do it before the flow runs, or
          // the flow starts on whatever an earlier test left in the shard's database.
          if (db) {
            const back = await hook("restore", { key, reset: true, parent });
            if (!back.ok || !back.body.reset) unknownDB = true;
          }
        }
      }
      if (outputs === undefined) {
        const inner: Frame = { step, last: key };
        frames.push(inner);
        try {
          outputs = (await def.run(args() as any)) ?? {};
        } finally {
          frames.pop();
        }
        for (const name of Object.keys(def.outputs ?? {})) {
          if (!(name in outputs)) throw new Error(`flow "${def.name}" declares the output "${name}" but run did not return it`);
        }
        if (look?.ok && look.body.save && !unknownDB) {
          const appOrigin = originOf(env.baseURL);
          const state = browser ? { ...(await env.context.storageState()), ...(appOrigin ? { appOrigin } : {}) } : undefined;
          const saved = await hook("save", { key, flow: def.name, source: def.source, inputs: hash, parent, browser, db, storageState: state, outputs, replace });
          if (saved.ok && saved.body.saved) checkpoint = "saved";
          else reason = saved.ok ? saved.body.reason : saved.body.error;
        }
      }
    } catch (e) {
      const ms = Date.now() - started;
      emit("flow.end", { step, outputs: {}, checkpoint: "none", ms, reason: String((e as Error).message ?? e).split("\n")[0].slice(0, 200) });
      emit("step.end", { step, ms, ok: false });
      throw e;
    }
    frame.last = key;
    const ms = Date.now() - started;
    emit("flow.end", { step, outputs: redact(outputs), checkpoint, ms, ...(reason ? { reason } : {}) });
    emit("step.end", { step, ms, ok: true });
    return outputs;
  }

  const caller = (async (which: Flow<any, any> | string, inputs: Record<string, unknown> = {}) => {
    const def = typeof which === "string" ? registry.get(which) : which;
    if (!def) throw new Error(`no flow named "${which}" (declared: ${[...registry.keys()].join(", ") || "none"}); is its file imported by a test?`);
    return call(def, inputs);
  }) as FlowCaller;
  return caller;
}

/** The test id of `info`, the way the reporter prints it. */
function idOf(info: TestInfo): string {
  return testId(info.project.name, relFile(info.config.rootDir, info.file), info.titlePath.slice(1));
}

/** The `flow` fixture: a `flow` function bound to the test's page, context and request. */
export async function flowFixture(
  fx: { page: Page; context: BrowserContext; request: APIRequestContext; baseURL?: string },
  use: (flow: FlowCaller) => Promise<void>,
  info: TestInfo,
): Promise<void> {
  await use(
    createFlowCaller({
      test: idOf(info),
      page: fx.page,
      context: fx.context,
      request: fx.request,
      baseURL: fx.baseURL,
      api: process.env.LOCALCI_API,
      token: process.env.LOCALCI_HOOK_TOKEN,
      report: process.env.LOCALCI_REPORT || undefined,
    }),
  );
}

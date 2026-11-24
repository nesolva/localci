import type { APIRequestContext, BrowserContext, Page, TestInfo } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { createFlowCaller, declaredFlows } from "./flows";
import { relFile, testId } from "./ids";

/**
 * Tenant isolation: with `isolation: tenant` every shard of a run shares one stack and one database, and the core
 * names a flow (`isolation_tenant_flow`) in `$LOCALCI_TENANT_FLOW` that makes the shard's tenant. This fixture runs
 * that flow once per test command, before its first test, and gives every test the flow's outputs.
 *
 * Playwright starts a new worker after a failing test, and a shard's serial group and reruns are several test
 * commands on one stack, so "once" is kept in the file the core names in `$LOCALCI_TENANT_FILE` (one per shard,
 * for the whole run; `tenant.json` in `$LOCALCI_OUTPUT_DIR` without it): `{flow, outputs}` once the flow ran,
 * `{flow, error}` when it failed. The core reads the same file: an error there ends the shard as an
 * infrastructure failure instead of a list of failed tests.
 *
 * Two workers of one shard can start together (a suite that sets `workers` above one, or a worker replaced while
 * another is still setting up), and the flow would run twice against the shard's one database. A lock file next to
 * the tenant file (`<file>.lock`, created exclusively, holding the pid of its owner) makes the first worker run the
 * flow while the others wait for its result. A lock whose owner is gone is taken over. The alternative, forcing
 * one worker from the reporter, would also serialize the tests of every shard.
 */
export const TENANT_FILE = "tenant.json";

/** The outputs of the tenant flow; `undefined` when the suite does not use tenant isolation. */
export type Tenant = Record<string, unknown> | undefined;

interface State {
  flow: string;
  outputs?: Record<string, unknown>;
  error?: string;
}

function read(file: string): State | undefined {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8"));
  } catch {
    return undefined;
  }
}

function write(file: string, state: State) {
  const tmp = `${file}.${process.pid}.tmp`;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(tmp, JSON.stringify(state), { mode: 0o600 });
  fs.renameSync(tmp, file);
}

/** The `tenant` fixture. Automatic, so the flow runs before the first test even when no test asks for the tenant. */
export async function tenantFixture(
  fx: { page: Page; context: BrowserContext; request: APIRequestContext },
  use: (tenant: Tenant) => Promise<void>,
  info: TestInfo,
): Promise<void> {
  const name = process.env.LOCALCI_TENANT_FLOW;
  const dir = process.env.LOCALCI_OUTPUT_DIR;
  const file = process.env.LOCALCI_TENANT_FILE || (dir ? path.join(dir, TENANT_FILE) : "");
  if (!name || !file) return use(undefined);

  let state = read(file);
  if (!state || state.flow !== name) {
    const got = await acquire(file, name);
    if (got.state) {
      state = got.state;
    } else {
      try {
        state = await makeTenant(name, file, fx, info);
      } finally {
        got.release?.();
      }
    }
  }
  if (state.error) throw new Error(`tenant flow "${name}" failed: ${state.error}`);
  await use(state.outputs ?? {});
}

const POLL_MS = 50;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "EPERM";
  }
}

/** Whether the lock's owner is gone: a dead pid, or no pid at all in a file that is not new. */
function stale(lockFile: string): boolean {
  try {
    const pid = Number(fs.readFileSync(lockFile, "utf8"));
    if (Number.isInteger(pid) && pid > 0) return !alive(pid);
    return Date.now() - fs.statSync(lockFile).mtimeMs > 10_000;
  } catch {
    return false; // gone already: the next try takes it
  }
}

function ownedBy(lockFile: string, pid: number): boolean {
  try {
    return fs.readFileSync(lockFile, "utf8") === String(pid);
  } catch {
    return false;
  }
}

/**
 * Removes a lock whose owner is gone and says whether it did. Several workers can find the same dead lock at once:
 * remove-then-create would let a slow one remove the lock another had just made. So the check and the removal are
 * done under a second lock, `<lock>.takeover`, created exclusively; the worker that gets it looks again (the lock
 * may be live by now) and the others go back to waiting. A takeover lock left by a worker that died mid-takeover
 * expires after a few seconds.
 */
function takeOver(lockFile: string): boolean {
  const guard = `${lockFile}.takeover`;
  try {
    fs.closeSync(fs.openSync(guard, "wx", 0o600));
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
    try {
      if (Date.now() - fs.statSync(guard).mtimeMs > 5_000) fs.rmSync(guard, { force: true });
    } catch {
      // gone already
    }
    return false;
  }
  try {
    if (!stale(lockFile)) return false;
    fs.rmSync(lockFile, { force: true });
    return true;
  } finally {
    fs.rmSync(guard, { force: true });
  }
}

/**
 * Either the tenant someone else made meanwhile (`state`), or the lock to make it under (`release` it when done).
 * Waits as long as the owner is alive: the flow's own time is bounded by the test's timeout.
 */
export async function acquire(file: string, name: string): Promise<{ state?: State; release?: () => void }> {
  const lockFile = `${file}.lock`;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  for (;;) {
    const done = read(file);
    if (done && done.flow === name) return { state: done };
    try {
      const fd = fs.openSync(lockFile, "wx", 0o600);
      fs.writeSync(fd, String(process.pid));
      fs.closeSync(fd);
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== "EEXIST") throw e;
      if (!takeOver(lockFile)) await sleep(POLL_MS);
      continue;
    }
    const release = () => {
      // Only our own lock: one that a takeover replaced belongs to the worker that made it.
      if (ownedBy(lockFile, process.pid)) fs.rmSync(lockFile, { force: true });
    };
    const raced = read(file); // the owner before us may have finished between our read and our lock
    if (raced && raced.flow === name) {
      release();
      return { state: raced };
    }
    return { release };
  }
}

async function makeTenant(
  name: string,
  file: string,
  fx: { page: Page; context: BrowserContext; request: APIRequestContext },
  info: TestInfo,
): Promise<State> {
  const fail = (message: string): State => {
    const state = { flow: name, error: message.split("\n")[0].slice(0, 200) };
    write(file, state);
    return state;
  };
  const def = declaredFlows().find((f) => f.name === name);
  if (!def) return fail(`no flow named "${name}" (is its file imported by a test?)`);
  // The tenant flow always runs: a restored checkpoint would hand this shard another shard's tenant.
  const flow = { ...def, checkpoint: {} };
  const known = { shard: Number(process.env.LOCALCI_SHARD) || 0, run: process.env.LOCALCI_RUN ?? "" } as Record<string, unknown>;
  const inputs = Object.fromEntries(Object.keys(def.inputs ?? {}).filter((k) => k in known).map((k) => [k, known[k]]));
  const call = createFlowCaller({
    test: testId(info.project.name, relFile(info.config.rootDir, info.file), info.titlePath.slice(1)),
    ...fx,
    api: process.env.LOCALCI_API,
    token: process.env.LOCALCI_HOOK_TOKEN,
    report: process.env.LOCALCI_REPORT || undefined,
  });
  try {
    const outputs = (await call(flow as any, inputs as any)) as Record<string, unknown>;
    const state = { flow: name, outputs };
    write(file, state);
    return state;
  } catch (e) {
    return fail(String((e as Error).message ?? e));
  }
}

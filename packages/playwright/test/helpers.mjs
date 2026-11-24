import { spawn, spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

export const here = path.dirname(fileURLToPath(import.meta.url));
export const sampleDir = path.resolve(here, "../../../examples/acme-shop");
export const schemaPath = path.resolve(here, "../../../protocol/v1.schema.json");

export function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
    s.on("error", reject);
  });
}

export function tmp(prefix) {
  return fs.mkdtempSync(path.join(os.tmpdir(), `localci-${prefix}-`));
}

/** Runs the sample suite once and returns its events. `args` go to `playwright test`. */
export async function runSample({ env = {}, args = [], report } = {}) {
  const dir = tmp("run");
  const file = report ?? path.join(dir, "report.ndjson");
  const port = String(await freePort());
  const r = spawnSync("npx", ["playwright", "test", ...args], {
    cwd: sampleDir,
    encoding: "utf8",
    env: { ...process.env, PORT: port, DB_PATH: path.join(dir, "db.json"), LOCALCI_REPORT: file, CI: "", ...env },
  });
  const events = fs.existsSync(file)
    ? fs.readFileSync(file, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l))
    : [];
  return { events, status: r.status, output: r.stdout + r.stderr };
}

/** Starts the sample app and resolves when it answers. */
export async function startApp(port, dbPath) {
  const child = spawn("node", ["server.js"], { cwd: sampleDir, env: { ...process.env, PORT: String(port), DB_PATH: dbPath }, stdio: "ignore" });
  for (let i = 0; i < 100; i++) {
    try {
      if ((await fetch(`http://127.0.0.1:${port}/healthz`)).ok) return child;
    } catch {}
    await new Promise((r) => setTimeout(r, 50));
  }
  child.kill();
  throw new Error("sample app did not start");
}

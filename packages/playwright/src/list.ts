import { spawnSync } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { testId, TITLE_SEP } from "./ids";

export interface ListedTest {
  test: string;
  file: string;
  title: string;
  line: number;
  project: string;
  titles: string[];
}

/** Set in the child process so a config that calls `withLocalci` does not list again. */
export const LISTING_ENV = "LOCALCI_LISTING";

interface JsonSuite {
  title: string;
  file?: string;
  line?: number;
  specs?: { title: string; file: string; line: number; tests: { projectName: string }[] }[];
  suites?: JsonSuite[];
}

/** Runs `playwright test --list` and returns every test with its localci id. `args` are extra CLI args (`-c file`, a project…). */
export function listTests(args: string[] = [], cwd = process.cwd()): ListedTest[] {
  const cli = require.resolve("@playwright/test/cli", { paths: [cwd] });
  const r = spawnSync(process.execPath, [cli, "test", "--list", "--reporter=json", ...args], {
    cwd,
    encoding: "utf8",
    maxBuffer: 256 * 1024 * 1024,
    env: { ...process.env, [LISTING_ENV]: "1", LOCALCI_TESTS: "", LOCALCI_REPORT: "" },
  });
  if (r.status !== 0 && !r.stdout) throw new Error(`playwright --list failed: ${r.stderr.trim() || `exit ${r.status}`}`);
  const json = JSON.parse(r.stdout) as { suites: JsonSuite[]; errors?: { message: string }[] };
  if (json.errors?.length) throw new Error(`playwright --list: ${json.errors[0].message}`);
  const out: ListedTest[] = [];
  const walk = (s: JsonSuite, describes: string[]) => {
    for (const spec of s.specs ?? []) {
      for (const t of spec.tests) {
        const titles = [...describes, spec.title];
        out.push({
          test: testId(t.projectName, spec.file, titles),
          file: spec.file,
          title: titles.join(TITLE_SEP),
          line: spec.line,
          project: t.projectName,
          titles,
        });
      }
    }
    for (const child of s.suites ?? []) walk(child, [...describes, child.title]);
  };
  // Top-level suites are files: their title is the file name, not a describe block.
  for (const file of json.suites) walk(file, []);
  return out;
}

/** A flow as `defineFlow` declared it. */
export interface ListedFlow {
  name: string;
  inputs: Record<string, string>;
  outputs: Record<string, string>;
  checkpoint: { browser?: boolean; db?: boolean };
  file: string | null;
}

/** The flows the test files declare: loads them the way `--list` does and collects what `defineFlow` wrote down. */
export function listFlows(args: string[] = [], cwd = process.cwd()): ListedFlow[] {
  const cli = require.resolve("@playwright/test/cli", { paths: [cwd] });
  const dump = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "localci-flows-")), "flows.ndjson");
  try {
    const r = spawnSync(process.execPath, [cli, "test", "--list", "--reporter=null", ...args], {
      cwd,
      encoding: "utf8",
      maxBuffer: 256 * 1024 * 1024,
      env: { ...process.env, [LISTING_ENV]: "1", LOCALCI_TESTS: "", LOCALCI_REPORT: "", LOCALCI_FLOWS_DUMP: dump },
    });
    if (r.status !== 0) throw new Error(`playwright --list failed: ${r.stderr.trim() || `exit ${r.status}`}`);
    const seen = new Map<string, ListedFlow>();
    if (fs.existsSync(dump)) {
      for (const line of fs.readFileSync(dump, "utf8").split("\n")) {
        if (line) {
          const f = JSON.parse(line) as ListedFlow;
          seen.set(f.name, f);
        }
      }
    }
    return [...seen.values()].sort((a, b) => (a.name < b.name ? -1 : 1));
  } finally {
    fs.rmSync(path.dirname(dump), { recursive: true, force: true });
  }
}

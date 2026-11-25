import type { PlaywrightTestConfig } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { LISTING_ENV, listTests } from "./list";

const escapeRe = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/** `-c file` / `--config file` of the running Playwright process, so the listing loads the same config. */
function configArgs(): string[] {
  const a = process.argv;
  for (let i = 0; i < a.length; i++) {
    if (a[i] === "-c" || a[i] === "--config") return a[i + 1] ? ["-c", a[i + 1]] : [];
    if (a[i].startsWith("--config=")) return ["-c", a[i].slice("--config=".length)];
  }
  return [];
}

/**
 * Only the tests whose ids are listed in the file `$LOCALCI_TESTS`. The listing runs in the process
 * that plans the run; workers and the listing child skip it.
 */
function onlyListed(file: string): Pick<PlaywrightTestConfig, "grep"> {
  const wanted = new Set(
    fs
      .readFileSync(file, "utf8")
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean),
  );
  const chosen = listTests(configArgs()).filter((t) => wanted.has(t.test));
  // `grep` sees "<project> <file relative to rootDir> <titles…>", each followed by the tags it declares: the
  // config's after the file, a describe's after its title, the test's after its own. The id check above made the
  // choice exact; after every part the pattern accepts this test's tags and no other word.
  const patterns = chosen.map((t) => {
    const tags = t.tags.length ? `(?: (?:${[...new Set(t.tags)].map(escapeRe).join("|")}))*` : "";
    const titles = t.titles.map((title) => escapeRe(title) + tags).join(" ");
    return `^\\s*${escapeRe(t.project)} \\S*${escapeRe(path.posix.basename(t.file))}${tags} ${titles}$`;
  });
  return { grep: patterns.length ? new RegExp(patterns.join("|")) : /^(?!)/ };
}

/**
 * Wraps a Playwright config so the run reports to localci: adds the reporter, keeps a screenshot and a
 * trace for failures (in a directory of its own per shard), and when `$LOCALCI_TESTS` is set runs only those test ids.
 */
export function withLocalci<T extends PlaywrightTestConfig>(config: T): T {
  const reporter = require.resolve("./reporter");
  const existing = config.reporter ?? (process.env.CI ? "dot" : "list");
  const reporters = Array.isArray(existing) ? existing : [[existing]];
  const tests = process.env.LOCALCI_TESTS;
  const filter =
    tests && !process.env[LISTING_ENV] && process.env.TEST_WORKER_INDEX === undefined ? onlyListed(tests) : {};
  // Shards of one run share a checkout, and Playwright empties its output directory when it starts: one
  // directory per shard keeps their screenshots and traces from deleting each other's.
  // localci passes a directory of its own per invocation (under the run's log directory, outside the
  // checkout), so reruns and the next run's `git clean` cannot delete the artifacts it has recorded.
  const shard = process.env.LOCALCI_SHARD;
  const given = process.env.LOCALCI_OUTPUT_DIR;
  const output = given
    ? { outputDir: given }
    : shard !== undefined ? { outputDir: path.posix.join(config.outputDir ?? "test-results", `shard-${shard}`) } : {};
  return {
    ...config,
    ...filter,
    ...output,
    reporter: [...reporters, [reporter]] as T["reporter"],
    use: { screenshot: "only-on-failure", trace: "retain-on-failure", ...config.use },
  };
}

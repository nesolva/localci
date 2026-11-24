#!/usr/bin/env node
import { listFlows, listTests } from "./list";

const [cmd, ...rest] = process.argv.slice(2);
if (cmd === "list") {
  try {
    for (const t of listTests(rest)) process.stdout.write(JSON.stringify({ test: t.test, file: t.file, title: t.title, line: t.line }) + "\n");
  } catch (e) {
    process.stderr.write(`localci-playwright: ${(e as Error).message}\n`);
    process.exit(2);
  }
} else if (cmd === "flows") {
  try {
    for (const f of listFlows(rest)) process.stdout.write(JSON.stringify(f) + "\n");
  } catch (e) {
    process.stderr.write(`localci-playwright: ${(e as Error).message}\n`);
    process.exit(2);
  }
} else {
  process.stderr.write("usage: localci-playwright list|flows [playwright args, e.g. -c playwright.config.ts]\n");
  process.exit(2);
}

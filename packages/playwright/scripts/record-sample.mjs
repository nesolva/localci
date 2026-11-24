// Re-records internal/report/testdata/sample.ndjson from a real run of the sample suite
// (ACME_BREAK=1, so it includes a failure). Machine paths are replaced by /work/acme-shop.
import * as fs from "node:fs";
import * as path from "node:path";
import { runSample, sampleDir, here } from "../test/helpers.mjs";

const { events, output } = await runSample({ env: { ACME_BREAK: "1" } });
if (events.length === 0) throw new Error(`no events recorded\n${output}`);
const root = path.resolve(sampleDir);
const text = events.map((e) => JSON.stringify(e)).join("\n").split(root).join("/work/acme-shop") + "\n";
const out = path.resolve(here, "../../../internal/report/testdata/sample.ndjson");
fs.mkdirSync(path.dirname(out), { recursive: true });
fs.writeFileSync(out, text);
console.log(`${events.length} events -> ${out}`);

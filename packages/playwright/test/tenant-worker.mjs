// One Playwright worker of a shard, as a process: runs the tenant fixture once and prints the tenant it got.
// The flow takes a while, so a second worker starting meanwhile finds it half done.
import * as fs from "node:fs";
import * as path from "node:path";
import { defineFlow } from "../dist/index.js";
import { tenantFixture } from "../dist/tenant.js";

const dir = process.env.WORKER_DIR;
defineFlow({
  name: "make-tenant-shared",
  outputs: { slug: "string" },
  run: async () => {
    fs.appendFileSync(path.join(dir, "flow.log"), `${process.pid}\n`);
    await new Promise((r) => setTimeout(r, 400));
    return { slug: `made-by-${process.pid}` };
  },
});
const info = { project: { name: "chromium" }, config: { rootDir: dir }, file: path.join(dir, "a.spec.ts"), titlePath: ["", "a test"] };
await tenantFixture({ page: {}, context: {}, request: {} }, async (tenant) => console.log(JSON.stringify(tenant)), info);

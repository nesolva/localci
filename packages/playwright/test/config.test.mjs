import assert from "node:assert/strict";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { withLocalci } = require("../dist/config.js");

test("LOCALCI_OUTPUT_DIR is the output directory when set, whatever the shard", () => {
  process.env.LOCALCI_SHARD = "0";
  process.env.LOCALCI_OUTPUT_DIR = "/logs/run/shard-0.rerun1";
  try {
    assert.equal(withLocalci({ outputDir: "test-results" }).outputDir, "/logs/run/shard-0.rerun1");
  } finally {
    delete process.env.LOCALCI_SHARD;
    delete process.env.LOCALCI_OUTPUT_DIR;
  }
});

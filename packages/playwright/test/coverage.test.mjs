import assert from "node:assert/strict";
import { test } from "node:test";
import { decodeMappings, filesOf, lookup, sourcePath } from "../dist/coverage.js";

test("mappings decode to the source each generated position came from", () => {
  // Line 0: col 0 → source 0; col 10 → source 1. Line 1: col 4 → source 1.
  const lines = decodeMappings("AAAA,UCAA;IAAA");
  const map = { sources: ["src/a.ts", "src/b.ts"], lines };
  assert.equal(lookup(map, 0, 3), 0);
  assert.equal(lookup(map, 0, 12), 1);
  assert.equal(lookup(map, 1, 9), 1);
  assert.equal(lookup(map, 1, 2), -1);
  assert.equal(lookup(map, 5, 0), -1);
});

test("source names lose bundler prefixes and dependencies are left out", () => {
  assert.equal(sourcePath("webpack://app/./src/cart.ts?x=1"), "src/cart.ts");
  assert.equal(sourcePath("file:///work/web/src/a.ts"), "/work/web/src/a.ts");
  assert.equal(sourcePath("../src/b.ts", "app"), "src/b.ts");
  assert.equal(sourcePath("webpack://app/./node_modules/react/index.js"), null);
  assert.equal(sourcePath("webpack://app/webpack/bootstrap"), null);
});

// A script `source` whose map is inline: line 0 → src/a.ts, line 1 → a dependency, line 2 → nothing.
function script(url, mapSources) {
  const body = "function a(){}\nfunction b(){}\nfunction c(){}\n";
  const map = { version: 3, sources: mapSources, mappings: "AAAA;ACAA" };
  const source = `${body}//# sourceMappingURL=data:application/json;base64,${Buffer.from(JSON.stringify(map)).toString("base64")}`;
  const fn = (startOffset) => ({ ranges: [{ startOffset, endOffset: startOffset + 14, count: 1 }] });
  return { url, source, functions: [fn(0), fn(15), fn(30)] };
}

test("code that ran and maps to no source makes the coverage incomplete, and dependencies do not", async () => {
  const context = {};
  const deps = await filesOf(context, [{ ...script("http://app.test/one.js", ["src/a.ts", "webpack://app/./node_modules/dep/x.js"]), functions: script("http://app.test/one.js", []).functions.slice(0, 2) }]);
  assert.deepEqual(deps, { files: ["src/a.ts"], incomplete: false });

  const hole = await filesOf(context, [script("http://app.test/two.js", ["src/a.ts", "webpack://app/./node_modules/dep/x.js"])]);
  assert.deepEqual(hole, { files: ["src/a.ts"], incomplete: true }, "the function on the unmapped line is a hole");

  const nameless = await filesOf(context, [{ ...script("http://app.test/three.js", ["src/a.ts", ""]), functions: script("http://app.test/three.js", []).functions.slice(0, 2) }]);
  assert.equal(nameless.incomplete, true, "a source with no name cannot be placed");
});

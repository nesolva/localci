import assert from "node:assert/strict";
import { test } from "node:test";
import { excerpt } from "../dist/failure-context.js";
import { matcherValues } from "../dist/reporter.js";

const tree = Array.from({ length: 200 }, (_, i) => `- item ${i}`).join("\n");

test("a long aria tree is cut to 40 lines that still contain the target", () => {
  for (const at of [0, 3, 100, 199]) {
    const out = excerpt(tree, `item ${at}`).split("\n");
    assert.equal(out.length, 40);
    assert.ok(out.includes(`- item ${at}`), `target ${at} inside the window`);
  }
});

test("a short tree is kept whole and an unknown target falls back to the top", () => {
  assert.equal(excerpt("- a\n- b", "b"), "- a\n- b");
  assert.equal(excerpt(tree, null).split("\n")[0], "- item 0");
});

test("expected and actual come from the matcher message", () => {
  const m = "expect(locator).toHaveText(expected) failed\n\nLocator: getByTestId('x')\nExpected: \"a\"\nReceived: \"b\"\nTimeout: 5000ms";
  assert.deepEqual(matcherValues(m), { expected: '"a"', actual: '"b"' });
  assert.deepEqual(matcherValues("Error: boom"), { expected: null, actual: null });
  assert.deepEqual(matcherValues("expect(received).toBe(expected)\n\nExpected: 3\nReceived: 2"), { expected: "3", actual: "2" });
});

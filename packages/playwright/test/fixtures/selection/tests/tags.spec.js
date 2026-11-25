const { test } = require("@playwright/test");

test("plain", () => {});

test("tagged", { tag: "@smoke" }, () => {});

test.describe("checkout", { tag: "@smoke" }, () => {
  test("pays", () => {});
  test("refunds", { tag: "@slow" }, () => {});
});

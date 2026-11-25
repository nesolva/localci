const { test } = require("@playwright/test");

test.describe(() => {
  test("inside an anonymous describe", () => {});
});

test.describe("", () => {
  test.describe("orders", () => {
    test("inside an empty title", () => {});
  });
});

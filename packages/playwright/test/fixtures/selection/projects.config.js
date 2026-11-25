const { defineConfig } = require("@playwright/test");
const { withLocalci } = require("../../../dist/config.js");

// A project with its own selection: Playwright uses a project's grep/grepInvert instead of the top-level ones.
module.exports = withLocalci(
  defineConfig({
    testDir: "tests",
    workers: 1,
    projects: [{ name: "all" }, { name: "smoke", grep: /@smoke/, grepInvert: /@slow/ }],
  }),
);

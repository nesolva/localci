const { defineConfig } = require("@playwright/test");
const { withLocalci } = require("../../../dist/config.js");

// `tag` tags every test of the suite: Playwright appends it right after the file name.
module.exports = withLocalci(defineConfig({ testDir: "tests", workers: 1, tag: "@acme" }));

const { defineConfig } = require("@playwright/test");
const { withLocalci } = require("../../../dist/config.js");

module.exports = withLocalci(defineConfig({ testDir: "tests", workers: 1 }));

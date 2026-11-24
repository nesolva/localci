import { defineConfig } from "@playwright/test";
import { withLocalci } from "@nesolva/localci-playwright";

const port = process.env.PORT ?? "4310";

export default withLocalci(
  defineConfig({
    testDir: "tests",
    workers: 1,
    use: { baseURL: `http://127.0.0.1:${port}` },
    // With `ACME_STACK=1` localci runs the server as the suite's stack, once for every shard.
    webServer: process.env.ACME_STACK === "1" ? undefined : {
      command: "node server.js",
      url: `http://127.0.0.1:${port}/healthz`,
      reuseExistingServer: false,
      env: { PORT: port, DB_PATH: process.env.DB_PATH ?? "" },
    },
  }),
);

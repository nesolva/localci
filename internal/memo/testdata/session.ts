import { existsSync, unlinkSync } from "node:fs";
import { defineFlow } from "@nesolva/localci-playwright";

/** Signs in like `login`, but its check can be told to refuse a restored session once (VERIFY_FAIL_MARKER). */
export const session = defineFlow({
  name: "session",
  inputs: { slug: "string" },
  outputs: { user: "string" },
  checkpoint: { browser: true, db: true },
  verify: async () => {
    const marker = process.env.VERIFY_FAIL_MARKER;
    if (marker && existsSync(marker)) {
      unlinkSync(marker);
      throw new Error("the restored session was refused");
    }
  },
  run: async ({ page, inputs }) => {
    await page.goto(`/t/${inputs.slug}/login`);
    await page.getByLabel("Username").fill("demo");
    await page.getByLabel("Password").fill("demo123");
    await page.getByRole("button", { name: "Sign in" }).click();
    await page.getByTestId("whoami").waitFor();
    return { user: "demo" };
  },
});

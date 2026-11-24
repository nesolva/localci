import { defineFlow } from "@nesolva/localci-playwright";

/** Signs the demo user in to a shop. The browser's cookies and the database (the session lives there) are kept. */
export const login = defineFlow({
  name: "login",
  inputs: { slug: "string", password: "string" },
  outputs: { user: "string" },
  checkpoint: { browser: true, db: true },
  // The restored session must still be accepted by the app; otherwise the flow signs in again.
  verify: async ({ context, inputs }) => {
    const res = await context.request.get(`/t/${inputs.slug}/api/me`);
    if (!res.ok()) throw new Error(`the restored session was refused (${res.status()})`);
  },
  run: async ({ page, inputs }) => {
    await page.goto(`/t/${inputs.slug}/login`);
    await page.getByLabel("Username").fill("demo");
    await page.getByLabel("Password").fill(inputs.password);
    await page.getByRole("button", { name: "Sign in" }).click();
    await page.getByTestId("whoami").waitFor();
    return { user: "demo" };
  },
});

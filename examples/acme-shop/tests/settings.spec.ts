import { expect, test } from "./fixtures";

// The tax rate lives in a settings file shared by every instance on the machine, outside the
// database: running this next to a test that reads totals makes both unreliable.
test("a global tax rate is added to the cart total", async ({ page, request, signedIn }) => {
  await page.getByRole("button", { name: "Add Widget to cart" }).click();
  await expect(page.getByTestId("cart-count")).toHaveText("1");
  try {
    await request.put("/api/global-settings", { data: { taxPercent: 10 } });
    await page.goto(signedIn.path("cart"));
    await expect(page.getByTestId("cart-total")).toHaveText("Total: $11.00");
    await expect(page.getByTestId("tax")).toHaveText("Tax 10% included");
  } finally {
    await request.put("/api/global-settings", { data: { taxPercent: 0 } });
  }
});

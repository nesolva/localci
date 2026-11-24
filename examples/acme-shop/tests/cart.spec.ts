import { expect, test } from "./fixtures";

test("adding a product updates the cart badge", async ({ page, signedIn }) => {
  await page.getByRole("button", { name: "Add Widget to cart" }).click();
  await expect(page.getByTestId("cart-count")).toHaveText("1");
});

test("two widgets and a gizmo add up", async ({ page, signedIn }) => {
  await test.step("add the products", async () => {
    await page.getByRole("button", { name: "Add Widget to cart" }).click();
    await expect(page.getByTestId("cart-count")).toHaveText("1");
    await page.getByRole("button", { name: "Add Gizmo to cart" }).click();
    await expect(page.getByTestId("cart-count")).toHaveText("2");
  });
  await test.step("raise the widgets to two", async () => {
    await page.goto(signedIn.path("cart"));
    await page.getByRole("button", { name: "More Widget" }).click();
    await expect(page.getByTestId("qty-widget")).toHaveText("2");
  });
  await test.step("check the total", async () => {
    await expect(page.getByTestId("cart-total")).toHaveText("Total: $27.50");
  });
});

test("removing the last line empties the cart", async ({ page, signedIn }) => {
  await page.getByRole("button", { name: "Add Gadget to cart" }).click();
  await expect(page.getByTestId("cart-count")).toHaveText("1");
  await page.goto(signedIn.path("cart"));
  await page.getByRole("button", { name: "Remove Gadget" }).click();
  await expect(page.getByTestId("empty")).toBeVisible();
});

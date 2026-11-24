import { expect, test } from "./fixtures";

test("the catalog lists products with prices", async ({ page, shop }) => {
  await page.goto(shop.path());
  const products = page.getByRole("table", { name: "Products" });
  await expect(products.getByRole("row")).toHaveCount(3);
  await expect(products.getByRole("row", { name: /Gadget/ })).toContainText("$25.00");
});

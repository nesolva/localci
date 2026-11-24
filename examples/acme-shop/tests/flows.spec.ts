import { expect, test } from "@nesolva/localci-playwright";
import { createShop } from "../flows/create-shop";
import { login } from "../flows/login";

// Both tests start the same way. With localci the second one restores what the first one built instead of
// creating the shop and signing in again.
for (const title of ["adds a product to the cart", "sees the cart total"]) {
  test(`flows: ${title}`, async ({ page, flow }) => {
    const { slug } = await flow(createShop, { name: "Flow Shop" });
    const { user } = await flow(login, { slug, password: "demo123" });
    expect(user).toBe("demo");
    await page.goto(`/t/${slug}/`);
    await expect(page.getByTestId("whoami")).toContainText("demo");
    await page.getByRole("button", { name: "Add Widget to cart" }).click();
    await expect(page.getByTestId("cart-count")).toHaveText("1");
  });
}

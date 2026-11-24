import { expect, test } from "@nesolva/localci-playwright";
import { createOrder } from "../flows/create-order";
import { createShop } from "../flows/create-shop";
import { login } from "../flows/login";

// Five orders, each checked four ways: the tests of one order share create-shop, login and create-order, and all
// twenty share create-shop and login.
const orders: [string, number][] = [["widget", 1], ["widget", 2], ["gadget", 1], ["gadget", 2], ["gizmo", 1]];
const checks = ["lists the order", "opens the order", "empties the cart", "keeps the session"];

for (const [product, qty] of orders) {
  for (const check of checks) {
    test(`memo: ${qty} x ${product} ${check}`, async ({ page, flow }) => {
      const { slug } = await flow(createShop, { name: "Memo Shop" });
      await flow(login, { slug, password: "demo123" });
      const { order } = await flow(createOrder, { slug, product, qty });
      if (check === "lists the order") {
        await page.goto(`/t/${slug}/orders`);
        await expect(page.getByRole("link", { name: `Order ${order}` })).toBeVisible();
      } else if (check === "opens the order") {
        await page.goto(`/t/${slug}/orders/${order}`);
        await expect(page.getByTestId("order-total")).toContainText("Paid $");
      } else if (check === "empties the cart") {
        await page.goto(`/t/${slug}/`);
        await expect(page.getByTestId("cart-count")).toHaveText("0");
      } else {
        await page.goto(`/t/${slug}/`);
        await expect(page.getByTestId("whoami")).toContainText("demo");
      }
    });
  }
}

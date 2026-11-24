import { expect, test } from "@nesolva/localci-playwright";
import { createOrder } from "../flows/create-order";
import { createShop } from "../flows/create-shop";
import { session } from "../flows/session";

// A chain two flows deep under a parent whose check can refuse its restored checkpoint: the tests under it must
// pass whether or not it does.
for (const qty of [1, 2, 3, 4]) {
  test(`memo-verify: order of ${qty}`, async ({ page, flow }) => {
    const { slug } = await flow(createShop, { name: "Verify Shop" });
    await flow(session, { slug });
    const { order } = await flow(createOrder, { slug, product: "widget", qty: 1 });
    await page.goto(`/t/${slug}/orders/${order}`);
    await expect(page.getByTestId("order-total")).toHaveText("Paid $10.00");
    expect(qty).toBeGreaterThan(0);
  });
}

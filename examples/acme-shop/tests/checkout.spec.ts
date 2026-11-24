import { expect, test } from "./fixtures";

async function fillCart(page: import("@playwright/test").Page, shop: { path: (p?: string) => string }) {
  await page.getByRole("button", { name: "Add Gadget to cart" }).click();
  await expect(page.getByTestId("cart-count")).toHaveText("1");
  await page.goto(shop.path("checkout"));
}

test("paying creates an order", async ({ page, signedIn }) => {
  await fillCart(page, signedIn);
  await page.getByLabel("Address").fill("1 Main Street");
  await page.getByRole("button", { name: "Pay" }).click();
  await expect(page.getByRole("heading", { name: "Thank you" })).toBeVisible();
  await expect(page.getByTestId("order-total")).toHaveText("Paid $25.00");
});

test("an order shows up in the order history", async ({ page, signedIn }) => {
  await fillCart(page, signedIn);
  await page.getByLabel("Address").fill("1 Main Street");
  await page.getByRole("button", { name: "Pay" }).click();
  await expect(page.getByRole("heading", { name: "Thank you" })).toBeVisible(); // leaving earlier aborts the payment on slow pages
  await page.goto(signedIn.path("orders"));
  await expect(page.getByRole("link", { name: "Order 1" })).toBeVisible();
});

test("an empty cart cannot be paid", async ({ page, signedIn }) => {
  await page.goto(signedIn.path("checkout"));
  await page.getByLabel("Address").fill("1 Main Street");
  await page.getByRole("button", { name: "Pay" }).click();
  await expect(page.getByTestId("pay-error")).toHaveText("Payment failed, try again.");
});

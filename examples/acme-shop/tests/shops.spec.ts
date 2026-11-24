import { expect, test } from "./fixtures";

test("create a shop from the home page", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("Shop name").fill("Corner Store");
  await page.getByRole("button", { name: "Create shop" }).click();
  await expect(page).toHaveURL(/\/t\/corner-store/);
  await expect(page.getByRole("heading", { name: "Corner Store" })).toBeVisible();
});

test("shops keep separate carts", async ({ page, request }) => {
  const other = await (await request.post("/api/tenants", { data: { name: "Other Shop" } })).json();
  const mine = await (await request.post("/api/tenants", { data: { name: "My Shop" } })).json();
  for (const [slug, product] of [[mine.slug, "Widget"], [other.slug, "Gizmo"]]) {
    await page.goto(`/t/${slug}/login`);
    await page.getByLabel("Username").fill("demo");
    await page.getByLabel("Password").fill("demo123");
    await page.getByRole("button", { name: "Sign in" }).click();
    await page.getByRole("button", { name: `Add ${product} to cart` }).click();
    await expect(page.getByTestId("cart-count")).toHaveText("1");
  }
  await page.goto(`/t/${mine.slug}/cart`);
  await expect(page.getByRole("cell", { name: "Widget", exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "Gizmo", exact: true })).toHaveCount(0);
});

import { test as base, expect, type Page } from "@nesolva/localci-playwright";
// Imported so the tenant flow is declared in every worker; it only runs under `isolation: tenant`.
import "../flows/create-tenant";

export interface Shop {
  slug: string;
  path: (p?: string) => string;
}

/**
 * A fresh tenant per test, so tests never depend on each other's data. Under `isolation: tenant` the shard's own
 * tenant (made once by the `create-tenant` flow) is used instead: the shard's tests share it, and tests that need
 * each test starts from a reset shop (carts, orders and sessions cleared).
 */
export const test = base.extend<{ shop: Shop; signedIn: Shop }>({
  shop: async ({ request, tenant }, use, info) => {
    if (typeof tenant?.slug === "string") {
      const slug = tenant.slug;
      await request.post(`/t/${slug}/api/reset`);
      return use({ slug, path: (p = "") => `/t/${slug}/${p}` });
    }
    const res = await request.post("/api/tenants", { data: { name: `Shop ${info.title}` } });
    const { slug } = await res.json();
    await use({ slug, path: (p = "") => `/t/${slug}/${p}` });
  },
  signedIn: async ({ shop, page }, use) => {
    await signIn(page, shop);
    await use(shop);
  },
});

export async function signIn(page: Page, shop: Shop, password = "demo123") {
  await page.goto(shop.path("login"));
  await page.getByLabel("Username").fill("demo");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
}

export { expect };

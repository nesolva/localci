import { defineFlow } from "@nesolva/localci-playwright";

/** Puts a product in the cart and pays. The order (database) and the session (browser) are kept. */
export const createOrder = defineFlow({
  name: "create-order",
  inputs: { slug: "string", product: "string", qty: "number" },
  outputs: { order: "string" },
  checkpoint: { browser: true, db: true },
  run: async ({ page, context, inputs }) => {
    await page.goto(`/t/${inputs.slug}/`);
    const add = await context.request.post(`/t/${inputs.slug}/api/cart`, { data: { product: inputs.product, delta: inputs.qty } });
    if (!add.ok()) throw new Error(`add to cart answered ${add.status()}: ${await add.text()}`);
    const res = await context.request.post(`/t/${inputs.slug}/api/pay`, { data: {} });
    if (!res.ok()) throw new Error(`pay answered ${res.status()}: ${await res.text()}`);
    const { order } = await res.json();
    return { order: String(order) };
  },
});

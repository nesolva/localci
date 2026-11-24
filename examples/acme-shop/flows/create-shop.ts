import { defineFlow } from "@nesolva/localci-playwright";

/** Makes a shop (a tenant) and returns its slug. Its database state is kept, so later tests skip the request. */
export const createShop = defineFlow({
  name: "create-shop",
  inputs: { name: "string" },
  outputs: { slug: "string" },
  checkpoint: { db: true },
  run: async ({ request, inputs }) => {
    const res = await request.post("/api/tenants", { data: { name: inputs.name } });
    const { slug } = await res.json();
    return { slug };
  },
});

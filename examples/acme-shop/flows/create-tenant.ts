import { defineFlow } from "@nesolva/localci-playwright";

/**
 * The tenant flow of `isolation: tenant`: every shard shares one stack and one database, and each makes its own
 * shop here. Declared without a checkpoint on purpose: it must run for real in every shard.
 */
export const createTenant = defineFlow({
  name: "create-tenant",
  inputs: { shard: "number" },
  outputs: { slug: "string" },
  run: async ({ request, inputs }) => {
    const res = await request.post("/api/tenants", { data: { name: `Shard ${inputs.shard}` } });
    if (!res.ok()) throw new Error(`POST /api/tenants answered ${res.status()}`);
    const { slug } = await res.json();
    return { slug };
  },
});

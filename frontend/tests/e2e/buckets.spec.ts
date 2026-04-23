import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// buckets.spec.ts exercises the bucket list CRUD flow: the page
// renders existing buckets and the "Create bucket" action POSTs to
// the tenant-scoped endpoint. Requires CONSOLE_E2E=1 and a running
// gateway; see helpers.ts. Tenant-scoped routes live under
// /api/tenants/{id}/... — see api/console/handler.go#Handler.Register.

requireGateway();

test.describe("buckets", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
  });

  test("lists buckets and exposes a create control", async ({ page }) => {
    await page.goto("/buckets");
    await expect(page.getByRole("heading", { name: /buckets/i })).toBeVisible();
    await expect(page.getByRole("button", { name: /create bucket/i })).toBeVisible();
  });

  test("submits POST to tenant-scoped buckets endpoint on create", async ({ page }) => {
    await page.goto("/buckets");
    // Attach the waiter before filling + clicking so the POST is
    // never missed. The form is inline — both inputs are required
    // by native HTML validation, so we fill them before submitting.
    const req = page.waitForRequest(
      (r) => /\/api\/tenants\/[^/]+\/buckets$/.test(r.url()) && r.method() === "POST",
      { timeout: 10_000 },
    );
    await page.getByLabel(/bucket name/i).fill("e2e-bucket");
    await page.getByLabel(/placement policy/i).fill("b2c_pooled_default");
    await page.getByRole("button", { name: /create bucket/i }).click();
    await req;
  });
});

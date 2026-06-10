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
    // The create control is the header "New bucket" button, which
    // opens the creation dialog (see BucketsPage.tsx).
    await expect(page.getByRole("button", { name: /new bucket/i }).first()).toBeVisible();
  });

  test("submits POST to tenant-scoped buckets endpoint on create", async ({ page }) => {
    await page.goto("/buckets");
    // Open the create dialog first — the form fields live inside it.
    await page.getByRole("button", { name: /new bucket/i }).first().click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    // Attach the waiter before filling + clicking so the POST is
    // never missed. Both inputs are required by native HTML
    // validation, so we fill them before submitting.
    const req = page.waitForRequest(
      (r) => /\/api\/tenants\/[^/]+\/buckets$/.test(r.url()) && r.method() === "POST",
      { timeout: 10_000 },
    );
    await dialog.getByLabel(/bucket name/i).fill("e2e-bucket");
    await dialog.getByLabel(/placement policy/i).fill("b2c_pooled_default");
    await dialog.getByRole("button", { name: /create bucket/i }).click();
    await req;
  });
});

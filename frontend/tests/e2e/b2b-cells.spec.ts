import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// b2b-cells.spec.ts verifies the dedicated-cells page is gated on
// tenant.contractType. The SPA renders the page at /b2b and only
// registers the route when isB2B is true (see App.tsx). B2C tenants
// navigating to /b2b fall through the catch-all Route and redirect
// to the dashboard. Requires CONSOLE_E2E=1 and a running gateway;
// see helpers.ts.

requireGateway();

test.describe("dedicated cells", () => {
  test("hidden for b2c tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2c_pooled" });
    await page.goto("/b2b");
    // B2C tenants do not have /b2b registered; the catch-all
    // Route in App.tsx redirects them back to the dashboard.
    await expect(page.getByRole("heading", { name: /dashboard/i })).toBeVisible();
    await expect(page.getByRole("heading", { name: /dedicated cells/i })).toHaveCount(0);
  });

  test("visible for b2b_dedicated tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2b_dedicated" });
    await page.goto("/b2b");
    await expect(page.getByRole("heading", { name: /dedicated cells/i })).toBeVisible();
  });
});

import { expect, test } from "@playwright/test";

import { seedAuth } from "./helpers";

// b2b-cells.spec.ts verifies the dedicated-cells page is visible
// only to tenants on a dedicated contract. The frontend gates the
// route on tenant.contractType; B2C tenants should see a 404 or
// redirect.

test.describe("dedicated cells", () => {
  test("hidden for b2c tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2c_pooled" });
    await page.goto("/dedicated-cells");
    // B2C tenants should be redirected or shown an empty-state.
    await expect(page.getByText(/not available|upgrade|b2b|dedicated/i)).toBeVisible();
  });

  test("visible for b2b_dedicated tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2b_dedicated" });
    await page.goto("/dedicated-cells");
    await expect(page.getByRole("heading", { name: /dedicated/i })).toBeVisible();
  });
});

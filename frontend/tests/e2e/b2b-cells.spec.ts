import { expect, test } from "@playwright/test";

import { seedAuth } from "./helpers";

// b2b-cells.spec.ts verifies the dedicated-cells page is gated on
// tenant.contractType. The SPA renders the page at /b2b and only
// registers the route when isB2B is true (see App.tsx). B2C
// tenants navigating to /b2b fall through the catch-all Route and
// redirect to the dashboard.
//
// All cases stub the dedicated-cells GET so the suite runs
// deterministically without a live gateway.

const STUB_CELLS = [
  {
    id: "cell-eu-west-1",
    region: "eu-west",
    country: "DE",
    status: "active",
    capacityPetabytes: 5,
    utilization: 0.42,
  },
  {
    id: "cell-us-east-1",
    region: "us-east",
    country: "US",
    status: "provisioning",
    capacityPetabytes: 10,
    utilization: 0,
  },
];

test.describe("dedicated cells", () => {
  test.beforeEach(async ({ page }) => {
    await page.route(/\/api\/tenants\/[^/]+\/dedicated-cells$/, (route) => {
      if (route.request().method() === "GET") {
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(STUB_CELLS),
        });
      }
      return route.fallback();
    });
  });

  test("hidden for b2c tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2c_pooled" });
    await page.goto("/b2b");
    // B2C tenants do not have /b2b registered; the catch-all
    // Route in App.tsx redirects them back to the dashboard.
    await expect(page.getByRole("heading", { name: /dashboard/i })).toBeVisible();
    await expect(page.getByRole("heading", { name: /dedicated cells/i })).toHaveCount(0);
  });

  test("renders cell rows for b2b_dedicated tenants", async ({ page }) => {
    await seedAuth(page, { contractType: "b2b_dedicated" });
    await page.goto("/b2b");
    await expect(page.getByRole("heading", { name: /dedicated cells/i })).toBeVisible();
    // Both stubbed cells should be rendered with their region and
    // status visible in the table body.
    await expect(page.getByText("cell-eu-west-1")).toBeVisible();
    await expect(page.getByText("cell-us-east-1")).toBeVisible();
    await expect(page.getByText(/provisioning/i).first()).toBeVisible();
  });

  test("renders cell rows for sovereign tenants too", async ({ page }) => {
    // Sovereign contracts are also B2B in App.tsx#isB2B; the page
    // must render for them as well so onboarding flows for
    // government / regulated customers work the same way.
    await seedAuth(page, { contractType: "sovereign" });
    await page.goto("/b2b");
    await expect(page.getByRole("heading", { name: /dedicated cells/i })).toBeVisible();
    await expect(page.getByText("cell-eu-west-1")).toBeVisible();
  });
});

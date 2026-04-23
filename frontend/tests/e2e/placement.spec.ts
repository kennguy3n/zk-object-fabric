import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// placement.spec.ts exercises the placement-policy editor. The
// SPA stores the canonical JSON Policy document in a textarea (see
// PlacementPolicyPage.tsx and ApiClient#savePlacementPolicy) and
// the save button PUTs it to the tenant-scoped placement endpoint.
// Requires CONSOLE_E2E=1 and a running gateway; see helpers.ts.

requireGateway();

// Matches the wire shape ApiClient#savePlacementPolicy parses back
// into placement_policy.Policy; anything else fails the editor's
// JSON.parse and never reaches the network.
const SAMPLE_POLICY = JSON.stringify(
  {
    tenant: "t-e2e",
    bucket: "",
    policy: {
      name: "p_country_strict",
      version: 1,
      allowed_countries: ["US"],
      replication_factor: 2,
    },
  },
  null,
  2,
);

test.describe("placement policies", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
    // Stub the tenant GET so PlacementPolicyPage populates its
    // `selected` state and enables the Save button. Without a
    // pre-existing policy the editor is in a clean-slate mode where
    // save is disabled; this mirrors what the production UI will
    // see after the backend has seeded at least one default policy
    // per tenant during onboarding.
    await page.route(/\/api\/tenants\/[^/]+\/placement$/, (route) => {
      if (route.request().method() !== "GET") {
        return route.fallback();
      }
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          tenant: "t-e2e",
          bucket: "",
          policy: { name: "p_default", version: 1 },
        }),
      });
    });
  });

  test("renders the placement editor", async ({ page }) => {
    await page.goto("/placement");
    await expect(page.getByRole("heading", { name: /placement/i })).toBeVisible();
  });

  test("saves a policy document via PUT to the tenant-scoped endpoint", async ({ page }) => {
    await page.goto("/placement");
    const editor = page.locator("textarea").first();
    await expect(editor).toBeVisible();
    await editor.fill(SAMPLE_POLICY);
    // Tenant-scoped routes live under /api/tenants/{id}/placement —
    // see api/console/handler.go#Handler.Register. Attach the
    // waiter before the click so the PUT isn't missed.
    const req = page.waitForRequest(
      (r) => /\/api\/tenants\/[^/]+\/placement$/.test(r.url()) && r.method() === "PUT",
      { timeout: 10_000 },
    );
    await page.getByRole("button", { name: /^save$/i }).click();
    await req;
  });
});

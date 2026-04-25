import { expect, test } from "@playwright/test";

import { seedAuth } from "./helpers";

// placement.spec.ts exercises the placement-policy editor. The
// SPA stores the canonical JSON Policy document in a textarea (see
// PlacementPolicyPage.tsx and ApiClient#savePlacementPolicy) and
// the save button PUTs it to the tenant-scoped placement endpoint.
//
// All cases stub the GET and PUT routes so the suite runs
// deterministically without a live gateway; the round-trip case
// also verifies that an editor save followed by a reload sees the
// just-saved policy reflected back.

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
    // Stub the PUT in addition to the beforeEach GET so the click
    // produces a deterministic 204 the SPA can render.
    await page.route(/\/api\/tenants\/[^/]+\/placement$/, (route) => {
      if (route.request().method() === "PUT") {
        return route.fulfill({ status: 204 });
      }
      return route.fallback();
    });
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

  test("round-trips: save then reload renders the saved policy", async ({ page }) => {
    // Capture the body the SPA PUTs and re-serve it on the next
    // GET so the reload sees what was saved. This mirrors the
    // production round-trip the Postgres-backed PlacementStore
    // serves (api/console/postgres_placement.go).
    let saved: string | null = null;
    await page.route(/\/api\/tenants\/[^/]+\/placement$/, async (route) => {
      const req = route.request();
      if (req.method() === "PUT") {
        saved = req.postData();
        return route.fulfill({ status: 204 });
      }
      if (req.method() === "GET") {
        if (saved) {
          return route.fulfill({
            status: 200,
            contentType: "application/json",
            body: saved,
          });
        }
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            tenant: "t-e2e",
            bucket: "",
            policy: { name: "p_default", version: 1 },
          }),
        });
      }
      return route.fallback();
    });

    await page.goto("/placement");
    const editor = page.locator("textarea").first();
    await editor.fill(SAMPLE_POLICY);
    const putReq = page.waitForRequest(
      (r) => /\/api\/tenants\/[^/]+\/placement$/.test(r.url()) && r.method() === "PUT",
      { timeout: 10_000 },
    );
    await page.getByRole("button", { name: /^save$/i }).click();
    await putReq;
    expect(saved).not.toBeNull();

    // Reload and confirm the editor renders the just-saved policy.
    // The SPA may pretty-print or compact the JSON; assert on a
    // distinctive substring (the country code we set above) rather
    // than the literal payload to keep the test robust to
    // formatting differences.
    await page.reload();
    const reloadedEditor = page.locator("textarea").first();
    await expect(reloadedEditor).toBeVisible();
    await expect(reloadedEditor).toContainText("p_country_strict");
    await expect(reloadedEditor).toContainText("US");
  });
});

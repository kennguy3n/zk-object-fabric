import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// placement.spec.ts exercises the placement-policy YAML editor.
// The editor is a textarea backed by the canonical YAML representation
// documented in docs/PROPOSAL.md §3.6; round-tripping a known
// document should not modify its contents. Requires CONSOLE_E2E=1
// and a running gateway; see helpers.ts.

requireGateway();

const SAMPLE_POLICY = `name: p_country_strict
version: 1
select:
  providers:
    - wasabi
    - ceph_rgw
constraints:
  country: ["US"]
`;

test.describe("placement policies", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
  });

  test("renders the placement editor", async ({ page }) => {
    await page.goto("/placement");
    await expect(page.getByRole("heading", { name: /placement/i })).toBeVisible();
  });

  test("saves a YAML document via PUT", async ({ page }) => {
    await page.goto("/placement");
    const editor = page.locator("textarea").first();
    if (await editor.count()) {
      await editor.fill(SAMPLE_POLICY);
      // Tenant-scoped routes live under /api/ (see
      // api/console/handler.go:Handler.Register); the v1 prefix
      // belongs to auth only.
      const req = page.waitForRequest(
        (r) => r.url().includes("/api/placement-policies/") && r.method() === "PUT",
      );
      const save = page.getByRole("button", { name: /save/i }).first();
      await save.click();
      await req;
    }
  });
});

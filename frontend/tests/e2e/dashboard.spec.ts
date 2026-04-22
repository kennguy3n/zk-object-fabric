import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// dashboard.spec.ts verifies that the dashboard renders the three
// usage stat cards (storage / requests / egress) and that the SSE
// stream wired in DashboardPage.tsx opens a connection to
// /api/v1/usage/stream/{tenantID}. Requires CONSOLE_E2E=1 and a
// running gateway; see helpers.ts.

requireGateway();

test.describe("dashboard", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
  });

  test("renders the three usage stat cards", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("heading", { name: /dashboard/i })).toBeVisible();
    await expect(page.getByText(/storage/i).first()).toBeVisible();
    await expect(page.getByText(/requests/i).first()).toBeVisible();
    await expect(page.getByText(/egress/i).first()).toBeVisible();
  });

  test("opens an SSE connection to /api/v1/usage/stream", async ({ page }) => {
    // EventSource uses fetch-like semantics under Playwright; we
    // assert on the request being issued rather than the stream
    // content so the test is independent of gateway uptime.
    const req = page.waitForRequest((r) => r.url().includes("/api/v1/usage/stream/"), {
      timeout: 10_000,
    });
    await page.goto("/");
    await req;
  });
});

import { expect, test } from "@playwright/test";

import { e2eEnabled, seedAuth } from "./helpers";

// error-states.spec.ts exercises the negative paths the SPA must
// surface gracefully: a wrong-password login, a 429 rate-limit
// response on a tenant-scoped read, and a 401 surfaced from a
// stale token. These tests stub /api responses so they run without
// a live gateway, which is the only way to exercise rate-limit and
// stale-token states deterministically. No CONSOLE_E2E gate is
// required for the stubbed cases; the live wrong-password test is
// gated explicitly.

test.describe("error states (stubbed)", () => {
  test("login surfaces 401 on wrong credentials", async ({ page }) => {
    await page.route(/\/api\/v1\/auth\/login$/, (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "invalid email or password" }),
      });
    });
    await page.goto("/login");
    await page.getByLabel(/email/i).fill("wrong@example.com");
    await page.getByLabel(/password/i).fill("definitely-not-the-password");
    await page.getByRole("button", { name: /sign in|login/i }).click();
    // The SPA renders auth errors inline in a panel with class
    // "danger-text" (LoginPage.tsx); assert the visible message
    // mentions the failure rather than coupling to exact copy.
    await expect(page.getByText(/invalid|incorrect|wrong|401|unauthorized/i)).toBeVisible({
      timeout: 5_000,
    });
    // The user must NOT be redirected to the dashboard on a 401.
    await expect(page).toHaveURL(/\/login$/);
  });

  test("buckets page surfaces 429 rate limit", async ({ page }) => {
    await seedAuth(page);
    let serverHits = 0;
    await page.route(/\/api\/tenants\/[^/]+\/buckets$/, (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      serverHits += 1;
      route.fulfill({
        status: 429,
        contentType: "application/json",
        headers: { "Retry-After": "5" },
        body: JSON.stringify({ error: "rate limit exceeded; retry after 5s" }),
      });
    });
    await page.goto("/buckets");
    await expect(page.getByText(/rate limit|429|too many requests|retry/i)).toBeVisible({
      timeout: 5_000,
    });
    expect(serverHits).toBeGreaterThan(0);
  });

  test("api-keys page surfaces 401 from a stale token", async ({ page }) => {
    await seedAuth(page);
    await page.route(/\/api\/tenants\/[^/]+\/keys$/, (route) => {
      if (route.request().method() !== "GET") return route.fallback();
      route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ error: "token expired" }),
      });
    });
    await page.goto("/api-keys");
    await expect(page.getByText(/expired|401|unauthorized|sign in/i)).toBeVisible({
      timeout: 5_000,
    });
  });
});

// liveLogin reproduces the wrong-password flow against a real
// gateway so a wrong-credentials regression is caught even when
// the route stub above hides a backend bug. The block is skipped
// unless CONSOLE_E2E is set so it does not break the unstubbed
// suite.
test.describe("error states (live)", () => {
  test.skip(!e2eEnabled, "CONSOLE_E2E=1 required for live error-state checks");

  test("login posts and renders the gateway's 401 message", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill(`nobody+${Date.now()}@example.com`);
    await page.getByLabel(/password/i).fill("wrong-password-1234567890");
    const resp = page.waitForResponse(
      (r) => r.url().includes("/api/v1/auth/login") && r.request().method() === "POST",
      { timeout: 15_000 },
    );
    await page.getByRole("button", { name: /sign in|login/i }).click();
    const r = await resp;
    expect(r.status()).toBe(401);
    await expect(page).toHaveURL(/\/login$/);
  });
});

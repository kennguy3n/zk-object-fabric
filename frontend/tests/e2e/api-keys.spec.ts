import { expect, test } from "@playwright/test";

import { requireGateway, seedAuth } from "./helpers";

// api-keys.spec.ts covers the API-key management page. The
// important invariant is that a freshly created key reveals the
// secret exactly once: subsequent listKeys reads return only the
// access key — mirroring the backend contract in
// api/console/handler.go:createAPIKey and the SPA's
// freshSecret-only-on-create state in ApiKeysPage.tsx.
//
// The "renders" + live-create cases require CONSOLE_E2E=1 against
// a running gateway. The "one-time reveal" case stubs the API so
// it runs deterministically in CI without a backend dependency.

test.describe("api keys (live)", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
  });

  test("renders the api-keys page", async ({ page }) => {
    requireGateway();
    await page.goto("/api-keys");
    await expect(page.getByRole("heading", { name: /api keys/i })).toBeVisible();
  });
});

test.describe("api keys (stubbed)", () => {
  test.beforeEach(async ({ page }) => {
    await seedAuth(page);
  });

  test("create reveals the secret exactly once and dismiss hides it", async ({ page }) => {
    let createCalls = 0;
    // Stub the initial GET so the page loads without a live
    // gateway, the POST so the create produces a deterministic
    // (access, secret) pair, and the second GET so the listing
    // mirrors the production behaviour where listKeys never
    // returns a secret.
    await page.route(/\/api\/tenants\/[^/]+\/keys$/, async (route) => {
      const method = route.request().method();
      if (method === "GET") {
        const body =
          createCalls === 0
            ? "[]"
            : JSON.stringify([
                {
                  accessKey: "AKIAE2EDEMO",
                  createdAt: new Date().toISOString(),
                },
              ]);
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body,
        });
      }
      if (method === "POST") {
        createCalls += 1;
        return route.fulfill({
          status: 201,
          contentType: "application/json",
          body: JSON.stringify({
            accessKey: "AKIAE2EDEMO",
            secretKey: "secretE2EONETIMEONLY1234567890",
            createdAt: new Date().toISOString(),
          }),
        });
      }
      return route.fallback();
    });

    await page.goto("/api-keys");
    await page.getByRole("button", { name: /create key|create|new/i }).first().click();

    // The "New key" reveal panel must show both the access and
    // the secret exactly once.
    await expect(page.getByText(/new key/i)).toBeVisible({ timeout: 5_000 });
    await expect(page.getByText("AKIAE2EDEMO").first()).toBeVisible();
    await expect(page.getByText("secretE2EONETIMEONLY1234567890")).toBeVisible();

    // Dismissing the reveal panel must hide the secret. The
    // listing row underneath continues to show the access key
    // because listKeys returns the descriptor without the secret.
    await page.getByRole("button", { name: /dismiss/i }).click();
    await expect(page.getByText("secretE2EONETIMEONLY1234567890")).toHaveCount(0);
    await expect(page.getByText("AKIAE2EDEMO").first()).toBeVisible();
  });
});

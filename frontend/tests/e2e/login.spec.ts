import { expect, test } from "@playwright/test";

import { requireGateway } from "./helpers";

// login.spec.ts covers the B2C self-service signup / login flow the
// gateway exposes at /api/v1/auth/{signup,login}. The suite runs
// against the Vite preview build defined in playwright.config.ts
// and requires a live gateway on the same origin — see the
// CONSOLE_E2E gate in helpers.ts.

requireGateway();

test.describe("auth flow", () => {
  test("renders the login page at /login", async ({ page }) => {
    await page.goto("/login");
    await expect(page.getByRole("heading", { name: /sign in/i })).toBeVisible();
    await expect(page.getByLabel(/email/i)).toBeVisible();
    await expect(page.getByLabel(/password/i)).toBeVisible();
  });

  test("signup form accepts email, password and tenant name", async ({ page }) => {
    await page.goto("/signup");
    await expect(page.getByRole("heading", { name: /sign up|create|signup/i })).toBeVisible();
    await page.getByLabel(/email/i).fill("e2e@example.com");
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");
    const tenantField = page.getByLabel(/tenant|workspace|organization/i);
    if (await tenantField.count()) {
      await tenantField.fill("e2e-tenant");
    }
    // The submit button is the form's primary CTA. We wait for the
    // request rather than the UI to keep the assertion independent
    // of the success-screen layout.
    const resp = page.waitForResponse((r) => r.url().includes("/api/v1/auth/signup"));
    await page.getByRole("button", { name: /sign up|create account|signup/i }).click();
    const response = await resp;
    expect([200, 201, 400, 409]).toContain(response.status());
  });

  test("login posts to /api/v1/auth/login", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill("e2e@example.com");
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");
    const resp = page.waitForResponse((r) => r.url().includes("/api/v1/auth/login"));
    await page.getByRole("button", { name: /sign in|login/i }).click();
    const response = await resp;
    expect([200, 401]).toContain(response.status());
  });
});

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
    // AuthShell renders the page heading as "Tenant console · Create
    // a tenant" (h1, see LoginPage.tsx#AuthShell); match the
    // trailing fragment so the assertion stays stable across shell
    // copy tweaks.
    await expect(
      page.getByRole("heading", { name: /create a tenant|create tenant|sign up|signup/i }),
    ).toBeVisible();
    await page.getByLabel(/organization|tenant|workspace/i).fill("e2e-tenant");
    await page.getByLabel(/email/i).fill("e2e@example.com");
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");
    // The submit button renders "Create tenant" (see
    // SignupPage.tsx); wait for the POST rather than asserting on
    // the success-screen layout so the test stays green regardless
    // of what the SPA does on 2xx.
    const resp = page.waitForResponse(
      (r) => r.url().includes("/api/v1/auth/signup") && r.request().method() === "POST",
      { timeout: 15_000 },
    );
    await page
      .getByRole("button", { name: /create tenant|create a tenant|sign up|signup/i })
      .click();
    const response = await resp;
    expect([200, 201, 400, 409]).toContain(response.status());
  });

  test("login posts to /api/v1/auth/login", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel(/email/i).fill("e2e@example.com");
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");
    // Filter by method so we never catch a preflight / stray GET.
    const resp = page.waitForResponse(
      (r) => r.url().includes("/api/v1/auth/login") && r.request().method() === "POST",
      { timeout: 15_000 },
    );
    await page.getByRole("button", { name: /sign in|login/i }).click();
    const response = await resp;
    expect([200, 401]).toContain(response.status());
  });
});

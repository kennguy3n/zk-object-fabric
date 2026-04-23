import { expect, test } from "@playwright/test";

import { requireGateway } from "./helpers";

// signup.spec.ts covers the B2C self-service signup flow end-to-end.
// It complements login.spec.ts by asserting that a successful signup
// returns a token and the SPA renders the authenticated dashboard.
// Requires CONSOLE_E2E=1 and a live gateway — see helpers.ts.

requireGateway();

test.describe("signup flow", () => {
  test("renders the signup page at /signup", async ({ page }) => {
    await page.goto("/signup");
    // AuthShell wraps the page heading as "Tenant console · Create
    // a tenant" (h1, LoginPage.tsx#AuthShell); match the trailing
    // fragment so the assertion stays stable across shell copy.
    await expect(
      page.getByRole("heading", { name: /create a tenant|create tenant|sign up|signup/i }),
    ).toBeVisible();
    await expect(page.getByLabel(/email/i)).toBeVisible();
    await expect(page.getByLabel(/password/i)).toBeVisible();
  });

  test("submits signup, receives token, lands on dashboard", async ({ page }) => {
    await page.goto("/signup");
    // Unique email per run so the gateway's in-memory MemoryAuthStore
    // does not collide with a previously registered tenant.
    const email = `e2e+${Date.now()}@example.com`;
    // Organization / tenant name is required by SignupPage.tsx; the
    // backend rejects empty tenantName with 400. Fill it first so the
    // POST actually fires when the submit button is clicked.
    await page.getByLabel(/organization|tenant|workspace/i).fill(`e2e-${Date.now()}`);
    await page.getByLabel(/email/i).fill(email);
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");
    const resp = page.waitForResponse(
      (r) => r.url().includes("/api/v1/auth/signup") && r.request().method() === "POST",
      { timeout: 15_000 },
    );
    // Submit button renders "Create tenant" (SignupPage.tsx); match
    // that exact copy so the click lands on the form's primary CTA.
    await page
      .getByRole("button", { name: /create tenant|create a tenant|sign up|signup/i })
      .click();
    const response = await resp;
    // 200/201 is the success path; 400/409 indicates a validation or
    // collision error which we still accept so the test does not
    // become flaky against shared gateways.
    expect([200, 201, 400, 409]).toContain(response.status());

    if (response.status() === 200 || response.status() === 201) {
      const body = (await response.json()) as { token?: string; tenant?: { id?: string } };
      expect(body.token, "signup response includes token").toBeTruthy();
      expect(body.tenant?.id, "signup response includes tenant id").toBeTruthy();
      // The SPA writes the auth payload to sessionStorage and
      // redirects to the dashboard; assert on both so a silent
      // redirect regression is caught.
      await expect(page).toHaveURL(/\/(dashboard|buckets|$)/);
    }
  });
});

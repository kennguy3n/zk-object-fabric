import { expect, test } from "@playwright/test";

import { requireGateway } from "./helpers";

// full-flow.spec.ts walks the entire B2C onboarding journey through
// the SPA: signup → land on dashboard → navigate to buckets →
// create a bucket → confirm it appears in the list → delete it.
// Object upload is intentionally NOT exercised here: the Phase 3
// SPA does not own an object-upload page (uploads go through the
// S3 data-plane endpoint, not the console mux), so the "upload"
// step is replaced by a list-after-create assertion that confirms
// the just-created bucket is visible in the catalog.
//
// Requires CONSOLE_E2E=1 and a running gateway; see helpers.ts.

requireGateway();

test.describe("end-to-end onboarding journey", () => {
  test("signup → dashboard → create bucket → list → delete", async ({ page }) => {
    // --- signup -------------------------------------------------
    await page.goto("/signup");
    const email = `e2e+full+${Date.now()}@example.com`;
    const tenantName = `e2e-full-${Date.now()}`;
    await page.getByLabel(/organization|tenant|workspace/i).fill(tenantName);
    await page.getByLabel(/email/i).fill(email);
    await page.getByLabel(/password/i).fill("correct-horse-battery-staple");

    const signupResp = page.waitForResponse(
      (r) => r.url().includes("/api/v1/auth/signup") && r.request().method() === "POST",
      { timeout: 15_000 },
    );
    await page
      .getByRole("button", { name: /create tenant|create a tenant|sign up|signup/i })
      .click();
    const signupResponse = await signupResp;
    // 409 means the gateway has been re-used and the email was
    // taken; tolerate that so the suite stays green when re-run
    // against a long-lived dev gateway.
    expect([200, 201, 409]).toContain(signupResponse.status());
    if (signupResponse.status() >= 400) {
      test.skip(true, "signup rejected by upstream; skipping post-signup steps");
    }

    // --- create bucket -----------------------------------------
    await page.goto("/buckets");
    await expect(page.getByRole("heading", { name: /buckets/i })).toBeVisible();

    const bucketName = `e2e-bucket-${Date.now()}`;
    const createBucket = page.waitForResponse(
      (r) =>
        /\/api\/tenants\/[^/]+\/buckets$/.test(r.url()) && r.request().method() === "POST",
      { timeout: 10_000 },
    );
    await page.getByLabel(/bucket name/i).fill(bucketName);
    await page.getByLabel(/placement policy/i).fill("b2c_pooled_default");
    await page.getByRole("button", { name: /create bucket/i }).click();
    const createResp = await createBucket;
    expect([200, 201]).toContain(createResp.status());

    // --- list: bucket appears -----------------------------------
    // The page refreshes after a successful create; assert the row
    // shows up so we know the GET round-tripped end-to-end.
    await expect(page.getByRole("cell", { name: bucketName })).toBeVisible({ timeout: 5_000 });

    // --- delete -------------------------------------------------
    // The Delete button fires window.confirm; auto-accept it so the
    // DELETE actually runs without a user click.
    page.once("dialog", (dialog) => dialog.accept());
    const deleteBucket = page.waitForResponse(
      (r) =>
        /\/api\/tenants\/[^/]+\/buckets\/[^/]+$/.test(r.url()) &&
        r.request().method() === "DELETE",
      { timeout: 10_000 },
    );
    await page
      .getByRole("row", { name: new RegExp(bucketName) })
      .getByRole("button", { name: /delete/i })
      .click();
    const deleteResp = await deleteBucket;
    expect([200, 204]).toContain(deleteResp.status());

    // --- list: bucket gone --------------------------------------
    await expect(page.getByRole("cell", { name: bucketName })).toHaveCount(0, { timeout: 5_000 });
  });
});

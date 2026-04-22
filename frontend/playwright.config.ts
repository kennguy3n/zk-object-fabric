import { defineConfig, devices } from "@playwright/test";

// Playwright config for the tenant console end-to-end suite. The
// specs under tests/e2e/ exercise the flows documented in
// docs/FRONTEND_PLAN.md §6 against a real Vite dev build backed by
// the gateway's in-memory console stores (see
// cmd/gateway/main.go:consoleTenantAdapter).
//
// PLAYWRIGHT_BASE_URL overrides the default so the same suite can
// run against a staging deploy in CI without editing the specs.
const baseURL = process.env.PLAYWRIGHT_BASE_URL ?? "http://127.0.0.1:4173";

export default defineConfig({
  testDir: "./tests/e2e",
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [["github"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL,
    trace: "on-first-retry",
    screenshot: "only-on-failure",
    video: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
    {
      name: "firefox",
      use: { ...devices["Desktop Firefox"] },
    },
  ],
  // `npm run test:e2e` boots a production-like preview server on
  // :4173 so the suite covers the same bundle the operators will
  // actually deploy. When PLAYWRIGHT_BASE_URL is set (e.g. a staging
  // URL) we skip the local server entirely.
  webServer: process.env.PLAYWRIGHT_BASE_URL
    ? undefined
    : {
        command: "npm run build && npm run preview -- --host 127.0.0.1 --port 4173",
        url: baseURL,
        reuseExistingServer: !process.env.CI,
        timeout: 120_000,
      },
});

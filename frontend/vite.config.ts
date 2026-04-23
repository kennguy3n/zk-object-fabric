/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the tenant console. The console talks to two
// separate surfaces on the gateway — tenant / usage / placement
// routes under /api/ and auth routes under /api/v1/auth/ — both of
// which live on port 8080 alongside the S3-compatible data plane.
// The dev proxy therefore forwards every /api request so SPA calls
// are same-origin and avoid CORS preflight noise.
// Canonical gateway dev port — see docs/FRONTEND_PLAN.md §6.
const GATEWAY_TARGET = "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: GATEWAY_TARGET,
        changeOrigin: true,
      },
    },
  },
  // `vite preview` is a static file server by default — it does NOT
  // inherit the dev-server proxy, so a production-preview build at
  // :4173 responds 405 to POST /api/v1/auth/login because the static
  // server does not know about auth routes. Mirror the dev proxy on
  // preview so the Playwright e2e suite (which boots the preview
  // build; see playwright.config.ts) and operators running the same
  // bundle locally both reach the gateway at :8080.
  preview: {
    port: 4173,
    proxy: {
      "/api": {
        target: GATEWAY_TARGET,
        changeOrigin: true,
      },
    },
  },
  test: {
    // The Playwright e2e scaffold under tests/e2e/ uses a different
    // runner (playwright test) and imports from @playwright/test,
    // which is not compatible with the vitest test API. Exclude it
    // here so `npm test` only runs the vitest unit suites.
    exclude: ["**/node_modules/**", "**/dist/**", "tests/e2e/**"],
  },
});

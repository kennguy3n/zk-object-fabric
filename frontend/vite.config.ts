/// <reference types="vitest" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite config for the tenant console. The console talks to two
// separate surfaces on the gateway — tenant / usage / placement
// routes under /api/ and auth routes under /api/v1/auth/ — both of
// which live on port 8080 alongside the S3-compatible data plane.
// The dev proxy therefore forwards every /api request so SPA calls
// are same-origin and avoid CORS preflight noise.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: "http://localhost:8080",
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
